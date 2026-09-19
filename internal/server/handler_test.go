package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/upstream"
)

// newTestHandler 构造 handler：上游指向 httptest 双服务器（enable/status 都不实际调上游，
// 但误触时会立刻失败而非打真站点），池用临时 state.json 装入 1 个账号。
func newTestHandler(t *testing.T, apiKey string) (*Handler, *pool.Pool) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	prevBase := upstream.ServerBase()
	upstream.SetServerBase(up.URL)
	// 包级模型缓存跨用例共享，重置避免相互影响
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		up.Close()
		upstream.SetServerBase(prevBase)
	})

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1"})
	return NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: apiKey}), p
}

// postEnable 发一次 enable 请求，bearer 为空表示不带 Authorization 头。
func postEnable(t *testing.T, h *Handler, uid, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/"+uid+"/enable", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEnableAccountEndpoint 禁用账号 → POST enable → 200 + 该账号脱敏状态，/status 同步反映。
func TestEnableAccountEndpoint(t *testing.T) {
	h, p := newTestHandler(t, "")
	p.Disable("u1", "session dead")

	rec := postEnable(t, h, "u1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var st pool.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v body=%s", err, rec.Body.String())
	}
	if st.UID != "u1" || st.Disabled || st.Reason != "" {
		t.Fatalf("want uid=u1 disabled=false, got %+v", st)
	}
	// 响应体必须是脱敏状态：不含任何令牌字段
	if raw := rec.Body.String(); strings.Contains(raw, "tok-1") {
		t.Fatalf("status body must be masked, got %s", raw)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /status: %v body=%s", err, rec.Body.String())
	}
	if len(body.Accounts) != 1 || body.Accounts[0].Disabled {
		t.Fatalf("/status must reflect the re-enable, got %+v", body.Accounts)
	}
}

// TestEnableAccountNotFound 未知 uid → 404 + code=account_not_found（沿用 OpenAI 错误体）。
func TestEnableAccountNotFound(t *testing.T) {
	h, _ := newTestHandler(t, "")

	rec := postEnable(t, h, "nobody", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v raw=%s", err, rec.Body.String())
	}
	if body.Error.Code != "account_not_found" {
		t.Fatalf("want code=account_not_found, got %q", body.Error.Code)
	}
	// 沿用 writeOpenAIError 形态（error-handling.md）
	if body.Error.Type != "api_error" || body.Error.Message == "" {
		t.Fatalf("want OpenAI error shape, got %+v", body.Error)
	}
}

// TestEnableAccountRouteMatrix 新路由不得 shadow / 破坏既有路由，且方法不符为 405
// （Go 1.22 方法模式；冲突模式在注册时就会 panic，这里锁住运行期行为）。
func TestEnableAccountRouteMatrix(t *testing.T) {
	h, _ := newTestHandler(t, "")
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/status", http.StatusOK},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/v1/models", http.StatusOK},
		{http.MethodPost, "/admin/accounts/u1/enable", http.StatusOK},
		{http.MethodGet, "/admin/accounts/u1/enable", http.StatusMethodNotAllowed},
		{http.MethodPost, "/admin/accounts/unknown/enable", http.StatusNotFound},
		{http.MethodPost, "/admin/accounts/u1/disable", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s: want %d, got %d body=%s", c.method, c.path, c.want, rec.Code, rec.Body.String())
		}
	}
}

// TestEnableAccountRequiresAPIKey 设置 APIKey 后与 /v1/* 同一鉴权：无/错 Bearer 401 且不改状态。
func TestEnableAccountRequiresAPIKey(t *testing.T) {
	h, p := newTestHandler(t, "k")
	p.Disable("u1", "session dead")

	if rec := postEnable(t, h, "u1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: want 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postEnable(t, h, "u1", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: want 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if st := p.List()[0]; !st.Disabled {
		t.Fatalf("rejected request must not change state, got %+v", st)
	}

	if rec := postEnable(t, h, "u1", "k"); rec.Code != http.StatusOK {
		t.Fatalf("correct bearer: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if st := p.List()[0]; st.Disabled {
		t.Fatalf("authorized call must enable the account, got %+v", st)
	}
}
