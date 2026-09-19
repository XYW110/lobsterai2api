package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
)

// newTestPool 返回使用临时 state.json 的池与其路径。
func newTestPool(t *testing.T) (*Pool, string) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "state.json")
	return New(fp), fp
}

// statusOfUID 取单个账号的对外状态，缺失即失败。
func statusOfUID(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("uid %s not in pool", uid)
	return Status{}
}

// readState 读回 state.json（缺失或非 JSON 即失败）。
func readState(t *testing.T, fp string) stateFile {
	t.Helper()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("state file not JSON: %v", err)
	}
	return sf
}

// TestSyncToDirReenablesOnTokenChange 重新登录（accessToken 变化）必须自动启用：
// 旧令牌非空且不同 → 清禁用/冷却/错误计数，Pick 能重新选中该账号。
func TestSyncToDirReenablesOnTokenChange(t *testing.T) {
	p, _ := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-old"})
	p.Disable("u1", "session dead")
	p.Cooldown("u1", CoolErr, time.Hour, "consecutive errors")
	p.NoteError("u1", 5, time.Hour) // errCount=1，未达阈值 → 仍在禁用态

	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "tok-new"}})

	st := statusOfUID(t, p, "u1")
	if st.Disabled || st.Cooling || st.ErrCount != 0 || st.Reason != "" {
		t.Fatalf("token change must re-enable and clear state, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("re-enabled account must be pickable, got %+v", got)
	}
}

// TestSyncToDirKeepsDisabledWhenTokenUnchanged 运维只是重写同一份文件（令牌相等）
// 不得解锁：否则 session 已死的账号会被放回池里反复失败。
func TestSyncToDirKeepsDisabledWhenTokenUnchanged(t *testing.T) {
	p, _ := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-same"})
	p.Disable("u1", "session dead")

	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "tok-same"}})

	st := statusOfUID(t, p, "u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Fatalf("unchanged token must stay disabled, got %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled account must not be picked, got %+v", got)
	}
}

// TestAddReenablesOnTokenChange Add 路径（启动装载）与 SyncToDir 同样适用恢复语义。
func TestAddReenablesOnTokenChange(t *testing.T) {
	p, _ := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-old"})
	p.Disable("u1", "session dead")

	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-new"})

	if st := statusOfUID(t, p, "u1"); st.Disabled || st.Reason != "" {
		t.Fatalf("changed non-empty token via Add must re-enable, got %+v", st)
	}
}

// TestSyncToDirReenablePersistsAcrossRestart 自动恢复必须同时落盘：只改内存的话，
// 重启后 load 读回旧 disabled=true，而占位保护会挡住新凭证再次触发恢复，
// 账号就此永久停用（本功能要消除的故障换个入口重现）。
func TestSyncToDirReenablePersistsAcrossRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-old"})
	p.Disable("u1", "session dead")

	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "tok-new"}})
	if st := statusOfUID(t, p, "u1"); st.Disabled {
		t.Fatalf("token change must re-enable in memory, got %+v", st)
	}
	if s := readState(t, fp).Accounts["u1"]; s.Disabled || s.Reason != "" {
		t.Fatalf("re-enable must be persisted to state.json, got %+v", s)
	}

	// 模拟进程重启：从 state.json 重建池，再装入同一份真实凭证
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "tok-new"})
	if st := statusOfUID(t, p2, "u1"); st.Disabled {
		t.Fatalf("re-enable must survive a restart, got %+v", st)
	}
	if got := p2.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("re-enabled account must be pickable after restart, got %+v", got)
	}
}

// TestSyncToDirRefreshKeepsDisabled 服务端自身 RefreshToken 是就地改写池内同一个
// auth 对象（upstream.RefreshToken 拿到指针后改字段），SaveAtomic 写回的也是新值；
// rescan 读回的令牌与内存一致，不得被误判成「重新登录」而把禁用账号放回池里。
func TestSyncToDirRefreshKeepsDisabled(t *testing.T) {
	p, _ := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-old"})
	p.Disable("u1", "session dead")

	// handler/scheduler 就是这么取凭证的：拿到的是 entry.a 本身
	a := p.AuthByUID("u1")
	a.AccessToken = "tok-refreshed"
	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "tok-refreshed"}})

	if st := statusOfUID(t, p, "u1"); !st.Disabled {
		t.Fatalf("server-side refresh must not re-enable, got %+v", st)
	}
}

// TestAddPlaceholderDoesNotReenable 占位保护：load() 塞入的 entry 是 accessToken 为空的
// 占位账号，启动后 Add 真实凭证不算「凭证变更」，否则每次重启都会清掉持久化的禁用状态。
func TestAddPlaceholderDoesNotReenable(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	seed := `{"accounts":{"u1":{"credits":42,"disabled":true,"reason":"session dead"}}}`
	if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed state file: %v", err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-new"})

	st := statusOfUID(t, p, "u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Fatalf("placeholder (empty old token) must not re-enable, got %+v", st)
	}
	if st.Credits != 42 {
		t.Fatalf("Add must keep loaded credits, got %d", st.Credits)
	}
}

// TestEnableClearsStateAndReportsHit Enable 命中：清禁用/冷却/错误计数并写盘，credits 不变；
// 未知 uid 返回 false 且不写盘。
func TestEnableClearsStateAndReportsHit(t *testing.T) {
	p, fp := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1"})
	p.SetCredits("u1", 1500)
	p.Disable("u1", "session dead")
	p.Cooldown("u1", CoolErr, time.Hour, "consecutive errors")
	p.NoteError("u1", 5, time.Hour) // errCount=1，未达阈值 → 仍冷却

	if !p.Enable("u1") {
		t.Fatal("Enable(u1) should report a hit")
	}
	st := statusOfUID(t, p, "u1")
	if st.Disabled || st.Cooling || st.ErrCount != 0 || st.Reason != "" {
		t.Fatalf("Enable must clear disabled/cooling/errCount, got %+v", st)
	}
	if st.Credits != 1500 {
		t.Fatalf("Enable must not touch credits, got %d", st.Credits)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("enabled account must be pickable, got %+v", got)
	}
	// 命中必须落盘：state.json 读回的也应是启用态
	if s := readState(t, fp).Accounts["u1"]; s.Disabled || !s.Until.IsZero() || s.Reason != "" {
		t.Fatalf("state.json not refreshed on hit: %+v", s)
	}

	// 未命中不得写盘：先删掉文件，若误写盘文件会重新出现
	if err := os.Remove(fp); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if p.Enable("nobody") {
		t.Fatal("Enable(unknown uid) should report a miss")
	}
	if _, err := os.Stat(fp); err == nil {
		t.Fatal("state file must not be rewritten on a miss")
	}
}
