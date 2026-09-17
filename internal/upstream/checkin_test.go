package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
)

// checkinEnv 签到测试环境：独立的版本接口 + 基址接口服务器。
type checkinEnv struct {
	updateSrv *httptest.Server
	baseSrv   *httptest.Server

	mu           sync.Mutex
	slotQuery    string
	slotUA       string
	ctxQuery     string
	checkinBody  string
	checkinCalls int
	ctxCalls     int
}

func newCheckinEnv(t *testing.T, versionResp string, handler http.HandlerFunc) *checkinEnv {
	t.Helper()
	env := &checkinEnv{}
	env.updateSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, versionResp)
	}))
	env.baseSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		defer env.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/slot"):
			env.slotQuery = r.URL.RawQuery
			env.slotUA = r.Header.Get("User-Agent")
		case strings.HasSuffix(r.URL.Path, "/context"):
			env.ctxQuery = r.URL.RawQuery
			env.ctxCalls++
		case strings.HasSuffix(r.URL.Path, "/check_in"):
			raw, _ := io.ReadAll(r.Body)
			env.checkinBody = string(raw)
			env.checkinCalls++
		}
		handler(w, r)
	}))
	// 指向测试双服务器并隔离版本缓存
	t.Setenv("LB2A_UPDATE_API", env.updateSrv.URL)
	prevBase := ServerBase()
	SetServerBase(env.baseSrv.URL)
	versionCache.mu.Lock()
	versionCache.v = ""
	versionCache.at = time.Time{}
	versionCache.mu.Unlock()
	t.Cleanup(func() {
		env.updateSrv.Close()
		env.baseSrv.Close()
		SetServerBase(prevBase)
	})
	return env
}

func checkinAcct() *auth.Auth { return &auth.Auth{AccessToken: "tok", UID: "u1"} }

// TestDailyCheckinSuccess 全流程：slot → context → check_in，断言查询参数与请求体。
func TestDailyCheckinSuccess(t *testing.T) {
	env := newCheckinEnv(t, `{"data":{"value":{"version":"1.4.2"}}}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/slot"):
				_, _ = io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"daily_2026","configRevision":7}}}`)
			case strings.HasSuffix(r.URL.Path, "/context"):
				_, _ = io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`)
			case strings.HasSuffix(r.URL.Path, "/check_in"):
				_, _ = io.WriteString(w, `{"code":0,"data":{"result":{"creditsGranted":100}}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

	c := New()
	if err := c.DailyCheckin(checkinAcct()); err != nil {
		t.Fatalf("DailyCheckin: %v", err)
	}
	if !strings.Contains(env.slotQuery, "clientVersion=1.4.2") ||
		!strings.Contains(env.slotQuery, "placement=desktop_sidebar") {
		t.Fatalf("slot query missing version/placement: %s", env.slotQuery)
	}
	if env.slotUA != "LobsterAI/1.4.2" {
		t.Fatalf("slot UA = %q, want LobsterAI/1.4.2", env.slotUA)
	}
	if !strings.Contains(env.ctxQuery, "configRevision=7") {
		t.Fatalf("context query missing configRevision: %s", env.ctxQuery)
	}
	if env.checkinCalls != 1 {
		t.Fatalf("check_in called %d times, want 1", env.checkinCalls)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(env.checkinBody), &body); err != nil {
		t.Fatalf("check_in body not JSON: %s", env.checkinBody)
	}
	if body["configRevision"] != float64(7) {
		t.Fatalf("check_in configRevision = %v, want 7", body["configRevision"])
	}
	key, _ := body["idempotencyKey"].(string)
	if len(key) != 36 || strings.Count(key, "-") != 4 {
		t.Fatalf("idempotencyKey not UUIDv4-shaped: %q", key)
	}
}

// TestDailyCheckinAlreadyClaimed claimedToday=true → 返回 nil 且不发 check_in。
func TestDailyCheckinAlreadyClaimed(t *testing.T) {
	env := newCheckinEnv(t, `{"data":{"value":{"version":"1.4.2"}}}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/slot") {
				_, _ = io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"daily_2026","configRevision":7}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":true},"actions":["check_in"]}}`)
		})

	c := New()
	if err := c.DailyCheckin(checkinAcct()); err != nil {
		t.Fatalf("DailyCheckin should treat claimedToday as success, got: %v", err)
	}
	if env.checkinCalls != 0 {
		t.Fatalf("check_in should not be called, called %d", env.checkinCalls)
	}
}

// TestDailyCheckinNoActivity slotState != available → 返回 nil，不走 context/check_in。
func TestDailyCheckinNoActivity(t *testing.T) {
	env := newCheckinEnv(t, `{"data":{"value":{"version":"1.4.2"}}}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"slotState":"paused"}}`)
		})

	c := New()
	if err := c.DailyCheckin(checkinAcct()); err != nil {
		t.Fatalf("DailyCheckin should treat no-activity as success, got: %v", err)
	}
	if env.ctxCalls != 0 || env.checkinCalls != 0 {
		t.Fatalf("context=%d check_in=%d calls, want 0/0", env.ctxCalls, env.checkinCalls)
	}
}

// TestDailyCheckinVersionFail 版本解析失败 → 报错且不触达签到端点。
func TestDailyCheckinVersionFail(t *testing.T) {
	newCheckinEnv(t, `{"code":500,"message":"boom"}`,
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("base server should not be reached, got %s", r.URL.Path)
		})

	c := New()
	err := c.DailyCheckin(checkinAcct())
	if err == nil {
		t.Fatal("expected error when version resolution fails")
	}
	if !strings.Contains(err.Error(), "resolve clientVersion") {
		t.Fatalf("error should mention version resolution: %v", err)
	}
}
