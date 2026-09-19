package scheduler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/upstream"
)

// schedEnv 调度器测试环境：版本接口 + 基址接口两个 httptest 服务器，
// 后者记录收到的请求 path，便于断言「跳过的账号零请求」。
type schedEnv struct {
	versionSrv *httptest.Server
	baseSrv    *httptest.Server

	mu    sync.Mutex
	paths []string
}

// newSchedEnv 启动假上游并把 upstream 基址指向它、LB2A_UPDATE_API 指向版本接口，
// t.Cleanup 还原（t.Setenv 自动还原环境变量）。
func newSchedEnv(t *testing.T, version string, h http.HandlerFunc) *schedEnv {
	t.Helper()
	env := &schedEnv{}
	env.versionSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"value":{"version":"`+version+`"}}}`)
	}))
	env.baseSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.paths = append(env.paths, r.Method+" "+r.URL.Path)
		env.mu.Unlock()
		h(w, r)
	}))
	t.Setenv("LB2A_UPDATE_API", env.versionSrv.URL)
	prev := upstream.ServerBase()
	upstream.SetServerBase(env.baseSrv.URL)
	t.Cleanup(func() {
		env.versionSrv.Close()
		env.baseSrv.Close()
		upstream.SetServerBase(prev)
	})
	return env
}

func (e *schedEnv) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.paths)
}

func (e *schedEnv) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.paths...)
}

// schedPool 建一个使用临时 state.json 的池并装入给定账号。
func schedPool(t *testing.T, accts ...*auth.Auth) *pool.Pool {
	t.Helper()
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	for _, a := range accts {
		p.Add(a)
	}
	return p
}

// schedStatus 取单个账号的对外状态，缺失即失败。
func schedStatus(t *testing.T, p *pool.Pool, uid string) pool.Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("uid %s not in pool", uid)
	return pool.Status{}
}

// TestNextFirePicksEarliestHour 多个整点里取最近的一个（纯函数，不依赖真实时钟）。
func TestNextFirePicksEarliestHour(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 19, 8, 30, 0, 0, loc)

	got := nextFire(now, []int{9, 21})
	want := time.Date(2026, 9, 19, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFire(08:30, [9 21]): want %s, got %s", want, got)
	}

	// 顺序不影响结果
	if got2 := nextFire(now, []int{21, 9}); !got2.Equal(want) {
		t.Fatalf("nextFire(08:30, [21 9]): want %s, got %s", want, got2)
	}
}

// TestNextFireBoundaryAtExactHour 恰好落在整点上时，该整点已过 → 顺延一天；
// 当天的其它更晚整点仍然优先。
func TestNextFireBoundaryAtExactHour(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, loc)

	got := nextFire(now, []int{9, 21})
	want := time.Date(2026, 9, 19, 21, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFire(09:00, [9 21]): want %s, got %s", want, got)
	}
}

// TestNextFireRollsToNextDay 当天所有整点都已过 → 滚动到次日的最近整点。
func TestNextFireRollsToNextDay(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 19, 23, 15, 0, 0, loc)

	got := nextFire(now, []int{9, 21})
	want := time.Date(2026, 9, 20, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFire(23:15, [9 21]): want %s, got %s", want, got)
	}

	// 单个整点同样滚动
	got = nextFire(now, []int{22})
	want = time.Date(2026, 9, 20, 22, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextFire(23:15, [22]): want %s, got %s", want, got)
	}
}

// TestRunCheckinNowUnfreezesAfterCredits 冷却账号签到后余额>0 必须解冻并刷新 credits
// （签到是解冻冷却账号的唯一自动路径）。
func TestRunCheckinNowUnfreezesAfterCredits(t *testing.T) {
	env := newSchedEnv(t, "9.9.9", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/slot"):
			// 无可用活动：DailyCheckin 的 best-effort 路径，返回 nil 且不阻断余额刷新
			_, _ = io.WriteString(w, `{"code":0,"data":{"slotState":"paused"}}`)
		case strings.HasSuffix(r.URL.Path, "/profile-summary"):
			_, _ = io.WriteString(w, `{"code":0,"data":{"totalCreditsRemaining":400}}`)
		default:
			http.NotFound(w, r)
		}
	})
	a := &auth.Auth{UID: "u1", AccessToken: "tok-1", RefreshToken: "rt-1"}
	p := schedPool(t, a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	if st := schedStatus(t, p, "u1"); !st.Cooling {
		t.Fatalf("setup: want a cooling account, got %+v", st)
	}

	New(Config{Pool: p, Upstream: upstream.New()}).RunCheckinNow()

	st := schedStatus(t, p, "u1")
	if st.Cooling {
		t.Fatalf("checkin with credits must unfreeze, got %+v", st)
	}
	if st.Credits != 400 || st.Reason != "" {
		t.Fatalf("credits must be refreshed from profile-summary, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("unfrozen account must be pickable, got %+v", got)
	}
	joined := strings.Join(env.calls(), " ")
	if !strings.Contains(joined, "profile-summary") {
		t.Fatalf("quota refresh must hit profile-summary, got %v", env.calls())
	}
}

// TestRunCheckinNowSkipsDisabled 禁用账号与缺 refreshToken 的账号都必须直接跳过，零请求。
func TestRunCheckinNowSkipsDisabled(t *testing.T) {
	env := newSchedEnv(t, "9.9.9", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	disabled := &auth.Auth{UID: "u1", AccessToken: "tok-1", RefreshToken: "rt-1"}
	noRefresh := &auth.Auth{UID: "u2", AccessToken: "tok-2"} // 无 refreshToken
	p := schedPool(t, disabled, noRefresh)
	p.Disable("u1", "session dead")

	New(Config{Pool: p, Upstream: upstream.New()}).RunCheckinNow()

	if got := env.callCount(); got != 0 {
		t.Fatalf("skipped accounts must trigger no request, got %d %v", got, env.calls())
	}
	if st := schedStatus(t, p, "u1"); !st.Disabled {
		t.Fatalf("disabled account state must not change, got %+v", st)
	}
}

// TestRunKeepaliveNowPersistsRefreshedToken 刷新成功 → 内存与磁盘 auth 文件都更新为新 token。
func TestRunKeepaliveNowPersistsRefreshedToken(t *testing.T) {
	const refreshed = "tok-refreshed"
	newSchedEnv(t, "9.9.9", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"`+refreshed+`","refreshToken":"rt-2","expiresIn":3600}}`)
	})

	fp := filepath.Join(t.TempDir(), "lobsterai-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "tok-old", RefreshToken: "rt-1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	p := schedPool(t, a)

	New(Config{Pool: p, Upstream: upstream.New()}).RunKeepaliveNow()

	if got := p.AuthByUID("u1").AccessToken; got != refreshed {
		t.Fatalf("in-memory token: want %q, got %q", refreshed, got)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth file: %v", err)
	}
	if parsed.AccessToken != refreshed || parsed.RefreshToken != "rt-2" {
		t.Fatalf("persisted token: want %q/%q, got %q/%q",
			refreshed, "rt-2", parsed.AccessToken, parsed.RefreshToken)
	}
	if parsed.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresIn must update ExpiresAt into the future, got %d", parsed.ExpiresAt)
	}
}

// TestRunKeepaliveNowDisablesOnSessionDead 刷新被签发方拒绝（ErrSessionDead）→ 账号禁用，
// 且不再出现在 Pick 结果里（只有新凭证或人工 Enable 能恢复）。
func TestRunKeepaliveNowDisablesOnSessionDead(t *testing.T) {
	env := newSchedEnv(t, "9.9.9", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":40100,"message":"refresh token was rejected"}`)
	})
	a := &auth.Auth{UID: "u1", AccessToken: "tok-1", RefreshToken: "rt-1"}
	p := schedPool(t, a)

	New(Config{Pool: p, Upstream: upstream.New()}).RunKeepaliveNow()

	st := schedStatus(t, p, "u1")
	if !st.Disabled {
		t.Fatalf("session-dead refresh must disable the account, got %+v", st)
	}
	if st.Reason != "refresh session dead" {
		t.Fatalf("want reason %q, got %q", "refresh session dead", st.Reason)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled account must not be pickable, got %+v", got)
	}
	if n := env.callCount(); n != 1 {
		t.Fatalf("want 1 refresh attempt, got %d %v", n, env.calls())
	}
}

// TestRunStopsOnContextCancel ctx 取消后 Run 必须立即返回，不留悬挂 goroutine。
func TestRunStopsOnContextCancel(t *testing.T) {
	// Run 在整点竞态下可能调用 RunCheckinNow/KeepaliveNow：给空池，避免 nil 依赖 panic
	s := New(Config{Pool: schedPool(t), Upstream: upstream.New()})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
