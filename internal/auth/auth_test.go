package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// nestedRaw 登录工具写出的嵌套形（{"auth":{...},"account":{...}}）。
const nestedRaw = `{
  "auth": {
    "accessToken": "at-nested",
    "refreshToken": "rt-nested",
    "expiresAt": 1700000000,
    "uuid": "uuid-nested",
    "firstKeyfrom": "fw-first",
    "latestKeyfrom": "fw-latest"
  },
  "account": {
    "uid": "uid-nested",
    "userId": "yid-nested",
    "nickname": "Nested Nick"
  }
}`

// flatRaw 手建的扁平形（{"accessToken":...,"uid":...}）。
const flatRaw = `{
  "accessToken": "at-flat",
  "refreshToken": "rt-flat",
  "expiresAt": 1700000001,
  "uuid": "uuid-flat",
  "firstKeyfrom": "fw-flat-first",
  "latestKeyfrom": "fw-flat-latest",
  "uid": "uid-flat",
  "userId": "yid-flat",
  "nickname": "Flat Nick"
}`

// writeAuthFile 落一个 auth 文件（0600）。
func writeAuthFile(t *testing.T, fp, raw string) {
	t.Helper()
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatalf("write %s: %v", fp, err)
	}
}

// assertOwnerOnlyMode 断言凭证文件为 0600。Windows 上 os.Stat 不报告 POSIX 权限位
// （Go 对可写文件一律合成 0666，权限由 ACL 控制），因此该平台跳过位断言。
func assertOwnerOnlyMode(t *testing.T, fp string) {
	t.Helper()
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("stat %s: %v", fp, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode: want 0600, got %04o", got)
	}
}

// TestParseNestedForm 嵌套形逐字段归一化（auth.* → 凭证，account.* → 账号身份）。
func TestParseNestedForm(t *testing.T) {
	a, err := Parse([]byte(nestedRaw))
	if err != nil {
		t.Fatalf("Parse nested: %v", err)
	}
	want := Auth{
		AccessToken:   "at-nested",
		RefreshToken:  "rt-nested",
		ExpiresAt:     1700000000,
		Uuid:          "uuid-nested",
		FirstKeyfrom:  "fw-first",
		LatestKeyfrom: "fw-latest",
		UID:           "uid-nested",
		UserId:        "yid-nested",
		Nickname:      "Nested Nick",
	}
	if *a != want {
		t.Fatalf("Parse nested:\n got  %+v\n want %+v", *a, want)
	}
}

// TestParseFlatForm 扁平形逐字段归一化（手建文件的兼容路径）。
func TestParseFlatForm(t *testing.T) {
	a, err := Parse([]byte(flatRaw))
	if err != nil {
		t.Fatalf("Parse flat: %v", err)
	}
	want := Auth{
		AccessToken:   "at-flat",
		RefreshToken:  "rt-flat",
		ExpiresAt:     1700000001,
		Uuid:          "uuid-flat",
		FirstKeyfrom:  "fw-flat-first",
		LatestKeyfrom: "fw-flat-latest",
		UID:           "uid-flat",
		UserId:        "yid-flat",
		Nickname:      "Flat Nick",
	}
	if *a != want {
		t.Fatalf("Parse flat:\n got  %+v\n want %+v", *a, want)
	}
}

// TestParseRejects 空输入/非法 JSON/缺 accessToken 各有稳定错误前缀，便于调用方分流。
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"empty input", "", "empty auth storage"},
		{"invalid JSON", `{"accessToken":`, "storage_parse_error"},
		{"JSON array not object", `[1,2,3]`, "storage_parse_error"},
		{"missing accessToken", `{"uid":"u1"}`, "parse_error"},
		{"blank accessToken", `{"accessToken":"   "}`, "parse_error"},
		{"nested missing accessToken", `{"auth":{"refreshToken":"rt"},"account":{"uid":"u1"}}`, "parse_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := Parse([]byte(c.raw))
			if err == nil {
				t.Fatalf("want error prefix %q, got nil (auth=%+v)", c.wantErr, a)
			}
			if !strings.HasPrefix(err.Error(), c.wantErr) {
				t.Fatalf("want error prefix %q, got %q", c.wantErr, err.Error())
			}
		})
	}
}

// TestNeedsRefresh 无 expiry 视为必须刷新；窗口内刷新；窗口外不刷新。
func TestNeedsRefresh(t *testing.T) {
	const within = 10 * time.Minute
	cases := []struct {
		name      string
		expiresAt int64
		want      bool
	}{
		{"no expiry", 0, true},
		{"negative expiry", -1, true},
		{"already expired", time.Now().Add(-time.Minute).Unix(), true},
		{"inside window", time.Now().Add(5 * time.Minute).Unix(), true},
		{"outside window", time.Now().Add(time.Hour).Unix(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{ExpiresAt: c.expiresAt}
			if got := a.NeedsRefresh(within); got != c.want {
				t.Fatalf("ExpiresAt=%d: want %v, got %v", c.expiresAt, c.want, got)
			}
		})
	}
}

// TestKeyfromBodyOptionalFields 必填键恒在；Uuid/UserId 为空时必须省略，不下发空值。
func TestKeyfromBodyOptionalFields(t *testing.T) {
	full := (&Auth{
		FirstKeyfrom:  "fk",
		LatestKeyfrom: "lk",
		Uuid:          "uu",
		UserId:        "yid",
	}).KeyfromBody()

	if full["version"] != "0.1.0" || full["firstKeyfrom"] != "fk" || full["latestKeyfrom"] != "lk" {
		t.Fatalf("base keyfrom body wrong: %+v", full)
	}
	if full["uuid"] != "uu" || full["userId"] != "yid" {
		t.Fatalf("optional fields must be present when set: %+v", full)
	}

	sparse := (&Auth{FirstKeyfrom: "fk"}).KeyfromBody()
	if _, ok := sparse["uuid"]; ok {
		t.Fatalf("empty Uuid must be omitted, got %+v", sparse)
	}
	if _, ok := sparse["userId"]; ok {
		t.Fatalf("empty UserId must be omitted, got %+v", sparse)
	}
	if _, ok := sparse["version"]; !ok {
		t.Fatalf("version must always be present, got %+v", sparse)
	}
	if len(sparse) != 3 {
		t.Fatalf("sparse body must have exactly 3 keys (firstKeyfrom/latestKeyfrom/version), got %+v", sparse)
	}
}

// TestSaveAtomicRoundTrip 写回后必须能被 Parse 读回同一份凭证、保持嵌套形，且不留 .tmp。
func TestSaveAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "lobsterai-uid-1.json")
	saved := Auth{
		AccessToken:   "at-new",
		RefreshToken:  "rt-new",
		ExpiresAt:     1800000000,
		Uuid:          "uuid-1",
		FirstKeyfrom:  "fw-1",
		LatestKeyfrom: "fw-2",
		UID:           "uid-1",
		UserId:        "yid-1",
		Nickname:      "Nick One",
		FilePath:      fp,
	}
	if err := saved.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	// 落盘必须是嵌套形：登录工具依赖该形态读取
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("saved file not JSON: %v raw=%s", err, raw)
	}
	for _, k := range []string{"auth", "account"} {
		if _, ok := probe[k]; !ok {
			t.Fatalf("saved file must use the nested shape, missing %q key: %s", k, raw)
		}
	}

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse round trip: %v", err)
	}
	want := saved
	want.FilePath = "" // Parse 不回填 FilePath，由 LoadDir 负责
	if *got != want {
		t.Fatalf("round trip:\n got  %+v\n want %+v", *got, want)
	}

	assertOwnerOnlyMode(t, fp)
	// tmp + rename：不得残留临时文件
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("atomic write must not leave tmp files, got %v", leftovers)
	}

	// 无 FilePath 必须报错且不落盘
	if err := (&Auth{AccessToken: "x"}).SaveAtomic(); err == nil {
		t.Fatal("SaveAtomic without FilePath must fail")
	}
}

// TestLoadDirSkipsBrokenFiles 坏 JSON 与不匹配 glob 的文件静默跳过，合法文件回填 FilePath。
func TestLoadDirSkipsBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	nestedPath := filepath.Join(dir, "lobsterai-aaa.json")
	flatPath := filepath.Join(dir, "lobsterai-bbb.json")
	writeAuthFile(t, nestedPath, nestedRaw)
	writeAuthFile(t, flatPath, flatRaw)
	writeAuthFile(t, filepath.Join(dir, "lobsterai-bad.json"), `{"accessToken":`)
	writeAuthFile(t, filepath.Join(dir, "other.json"), flatRaw) // 不匹配 lobsterai-*.json

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 loadable auths (broken/non-matching skipped), got %d: %+v", len(got), got)
	}
	byUID := make(map[string]*Auth, len(got))
	for _, a := range got {
		byUID[a.UID] = a
	}
	if a := byUID["uid-nested"]; a == nil {
		t.Fatalf("nested form not loaded, got %+v", byUID)
	} else if a.FilePath != nestedPath {
		t.Fatalf("nested FilePath: want %q, got %q", nestedPath, a.FilePath)
	} else if a.AccessToken != "at-nested" {
		t.Fatalf("nested AccessToken: want %q, got %q", "at-nested", a.AccessToken)
	}
	if a := byUID["uid-flat"]; a == nil {
		t.Fatalf("flat form not loaded, got %+v", byUID)
	} else if a.FilePath != flatPath {
		t.Fatalf("flat FilePath: want %q, got %q", flatPath, a.FilePath)
	}

	// 目录不存在时 best-effort：返回空且不报错
	if empty, err := LoadDir(filepath.Join(t.TempDir(), "missing")); err != nil || len(empty) != 0 {
		t.Fatalf("missing dir: want 0 auths and nil error, got %d auths, err=%v", len(empty), err)
	}
}
