package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// ---------------------------------------------------------------------------
// 假上游夹具
// ---------------------------------------------------------------------------

// fakeUpstream 是可观测的假上游：记录每次请求的 path 与最近一次 chat 请求体。
type fakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	chatReq []byte
}

// newFakeUpstream 启动假上游并接管 upstream 基址与包级模型缓存。
// 基址与缓存在 t.Cleanup 还原/清空，避免用例互相污染（见 design.md）。
func newFakeUpstream(t *testing.T, h http.HandlerFunc) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{}
	fu.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fu.mu.Lock()
		fu.paths = append(fu.paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/proxy/v1/chat/completions" {
			fu.chatReq = raw
		}
		fu.mu.Unlock()
		h(w, r)
	}))
	prevBase := upstream.ServerBase()
	upstream.SetServerBase(fu.srv.URL)
	resetDynamicModelsCache()
	t.Cleanup(func() {
		fu.srv.Close()
		upstream.SetServerBase(prevBase)
		resetDynamicModelsCache()
	})
	return fu
}

// resetDynamicModelsCache 清空包级 1h 模型缓存（跨用例共享，必须显式重置）。
func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
}

func (u *fakeUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}

func (u *fakeUpstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func (u *fakeUpstream) lastChatBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.chatReq...)
}

// newChatHandler 构造带账号的 handler：账号 ExpiresAt 设在未来，避免 handler
// 走 refresh 分支（那会额外打 /api/auth/refresh，不是这些用例的关注点）。
func newChatHandler(t *testing.T, apiKey string, credits map[string]int64) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	for uid, cr := range credits {
		p.Add(&auth.Auth{
			UID:         uid,
			AccessToken: "tok-" + uid,
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		})
		p.SetCredits(uid, cr)
	}
	return NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: apiKey}), p
}

// poolStatus 取单个账号的对外状态，缺失即失败。
func poolStatus(t *testing.T, p *pool.Pool, uid string) pool.Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("uid %s not in pool", uid)
	return pool.Status{}
}

// ---------------------------------------------------------------------------
// 路由与鉴权
// ---------------------------------------------------------------------------

// TestHealthz 健康检查返回 200 "ok"（容器 healthcheck 依赖该字面量）。
func TestHealthz(t *testing.T) {
	h, _ := newTestHandler(t, "secret") // 无鉴权：即使设了 APIKey 也必须可访问

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("want body %q, got %q", "ok", got)
	}
}

// TestStatusShape /status 返回 {"accounts":[...]}，按 uid 排序、字段来自 pool、脱敏且不鉴权。
func TestStatusShape(t *testing.T) {
	h, p := newTestHandler(t, "secret") // /status 设计上不鉴权，设了 APIKey 也必须可访问
	p.Add(&auth.Auth{UID: "u2", Nickname: "Two"})
	p.SetCredits("u1", 1500)
	p.Disable("u2", "session dead")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /status: %v body=%s", err, rec.Body.String())
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d: %+v", len(body.Accounts), body.Accounts)
	}
	if body.Accounts[0].UID != "u1" || body.Accounts[1].UID != "u2" {
		t.Fatalf("accounts must be sorted by uid, got %+v", body.Accounts)
	}
	if body.Accounts[0].Credits != 1500 || body.Accounts[0].Disabled {
		t.Fatalf("u1 status wrong, got %+v", body.Accounts[0])
	}
	if !body.Accounts[1].Disabled || body.Accounts[1].Reason != "session dead" {
		t.Fatalf("u2 status wrong, got %+v", body.Accounts[1])
	}
	if raw := rec.Body.String(); strings.Contains(raw, "tok-1") {
		t.Fatalf("status body must be masked, got %s", raw)
	}
}

// TestWithAuthRejectsMissingKey 无 / 错 / 非 Bearer 形式的凭证一律 401，且不触达上游。
func TestWithAuthRejectsMissingKey(t *testing.T) {
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	h, _ := newChatHandler(t, "secret", map[string]int64{"u1": 10})

	cases := []struct{ name, bearer string }{
		{"missing header", ""},
		{"wrong key", "Bearer nope"},
		{"not bearer scheme", "Basic secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[]}`))
			if c.bearer != "" {
				req.Header.Set("Authorization", c.bearer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d body=%s", rec.Code, rec.Body.String())
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
			if body.Error.Code != "invalid_api_key" || body.Error.Type != "api_error" {
				t.Fatalf("want invalid_api_key in OpenAI shape, got %+v", body.Error)
			}
		})
	}
	if got := fu.callCount(); got != 0 {
		t.Fatalf("rejected requests must not reach upstream, got %d calls %v", got, fu.calls())
	}
}

// TestWithAuthAcceptsKey 正确 Bearer 放行并字节级透传上游流。
func TestWithAuthAcceptsKey(t *testing.T) {
	const stream = "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	h, _ := newChatHandler(t, "secret", map[string]int64{"u1": 10})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != stream {
		t.Fatalf("authorized request must stream through:\n got  %q\n want %q", got, stream)
	}
	if got := fu.callCount(); got != 1 {
		t.Fatalf("want exactly 1 upstream call, got %d %v", got, fu.calls())
	}
}

// ---------------------------------------------------------------------------
// chat completions：聚合 / 透传 / 轮换 / 无可用账号
// ---------------------------------------------------------------------------

// TestChatCompletionsNonStreamingAggregates 非流式客户端：把 SSE 分片聚合成单个
// chat.completion；同时锁住「上游只支持 SSE」——客户端传 stream=false，转发时必须改 true。
func TestChatCompletionsNonStreamingAggregates(t *testing.T) {
	const stream = "data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	h, _ := newChatHandler(t, "", map[string]int64{"u1": 10})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode aggregated response: %v body=%s", err, rec.Body.String())
	}
	if resp.ID != "cmpl-1" || resp.Object != "chat.completion" || resp.Model != "deepseek-v4-pro" {
		t.Fatalf("response envelope wrong: %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d: %+v", len(resp.Choices), resp.Choices)
	}
	c := resp.Choices[0]
	if c.Message.Content != "Hello world" || c.Message.Role != "assistant" || c.FinishReason != "stop" {
		t.Fatalf("aggregation wrong: %+v", c)
	}
	// 上游只支持流式（stream:false 实测返回 500）：客户端传 false 时转发必须改成 true
	if got := string(fu.lastChatBody()); !strings.Contains(got, `"stream":true`) {
		t.Fatalf("upstream request must force stream=true, got %s", got)
	}
}

// TestChatCompletionsStreamByteExact 流式路径逐字节透传（含 >4KB 窥探窗口的多帧流），
// 且末尾 [DONE] 不得被重复追加。
func TestChatCompletionsStreamByteExact(t *testing.T) {
	var want bytes.Buffer
	want.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
	for i := 0; i < 80; i++ { // 每帧 ~130B，80 帧 ≈ 10KB > peekBufSize(4KB)
		want.WriteString(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"`)
		want.WriteString(strings.Repeat("y", 60))
		want.WriteString("\"}}]}\n\n")
	}
	want.WriteString("data: [DONE]\n\n")

	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(want.Bytes())
	})
	h, _ := newChatHandler(t, "", map[string]int64{"u1": 10})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), want.Bytes()) {
		t.Fatalf("stream not byte-exact: got %d bytes, want %d", rec.Body.Len(), want.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("want Content-Type text/event-stream, got %q", ct)
	}
	if got := fu.lastChatBody(); !strings.Contains(string(got), `"stream":true`) {
		t.Fatalf("upstream request must force stream=true, got %s", got)
	}
}

// TestChatCompletionsRotatesOnErrorFrame HTTP 200 里藏 40201 错误帧：该账号必须被
// 硬冷却，请求轮换到下一账号并成功返回（error-handling.md 的三态契约）。
func TestChatCompletionsRotatesOnErrorFrame(t *testing.T) {
	const errFrame = "event:error\n" +
		"data: {\"type\":\"error\",\"error\":{\"code\":40201,\"message\":\"免费额度已用完，请升级套餐\"}}\n\n"
	const okStream = "data: {\"id\":\"c2\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.Header.Get("Authorization"), "tok-u1") {
			_, _ = io.WriteString(w, errFrame)
			return
		}
		_, _ = io.WriteString(w, okStream)
	})
	// credits 固定 Pick 顺序：u1 先被选中
	h, p := newChatHandler(t, "", map[string]int64{"u1": 900, "u2": 100})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("rotation must recover: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rotated response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "recovered" {
		t.Fatalf("second account must serve the response, got %s", rec.Body.String())
	}

	st1 := poolStatus(t, p, "u1")
	if !st1.Cooling || st1.Reason == "" {
		t.Fatalf("error-frame account must be cooled with a reason, got %+v", st1)
	}
	if st2 := poolStatus(t, p, "u2"); st2.Cooling {
		t.Fatalf("serving account must stay healthy, got %+v", st2)
	}
	if got := fu.callCount(); got != 2 {
		t.Fatalf("want 2 upstream attempts (rotate once), got %d %v", got, fu.calls())
	}
}

// TestChatCompletionsNoHealthyAccount 无可用账号 → 503 no_healthy_account，且不打上游。
func TestChatCompletionsNoHealthyAccount(t *testing.T) {
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	h, p := newChatHandler(t, "", map[string]int64{"u1": 10})
	p.Disable("u1", "session dead")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
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
	if body.Error.Code != "no_healthy_account" || body.Error.Type != "api_error" || body.Error.Message == "" {
		t.Fatalf("want no_healthy_account in OpenAI shape, got %+v", body.Error)
	}
	if got := fu.callCount(); got != 0 {
		t.Fatalf("disabled account must not trigger upstream calls, got %d %v", got, fu.calls())
	}
}

// ---------------------------------------------------------------------------
// 模型列表
// ---------------------------------------------------------------------------

// TestModelsFallsBackToStaticOnUpstreamFailure 动态接口失败 → 回退静态表（19 条）。
func TestModelsFallsBackToStaticOnUpstreamFailure(t *testing.T) {
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":500,"message":"boom"}`)
	})
	h, _ := newChatHandler(t, "", map[string]int64{"u1": 10})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode models: %v body=%s", err, rec.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("want object=list, got %q", body.Object)
	}
	if len(body.Data) != len(staticModels) {
		t.Fatalf("want %d static models, got %d", len(staticModels), len(body.Data))
	}
	if body.Data[0].ID != staticModels[0]["id"] {
		t.Fatalf("first static model: want %v, got %q", staticModels[0]["id"], body.Data[0].ID)
	}
	if got := fu.callCount(); got != 1 {
		t.Fatalf("want 1 upstream attempt before fallback, got %d %v", got, fu.calls())
	}
}

// TestModelsCachesDynamicList 动态接口成功 → 用动态列表，并在 1h 内命中缓存不再打上游。
func TestModelsCachesDynamicList(t *testing.T) {
	fu := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/available" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":[{"modelId":"dyn-a"},{"modelId":"dyn-b"}]}`)
	})
	h, _ := newChatHandler(t, "", map[string]int64{"u1": 10})

	fetch := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode models: %v body=%s", err, rec.Body.String())
		}
		ids := make([]string, 0, len(body.Data))
		for _, m := range body.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}

	got := fetch()
	if len(got) != 2 || got[0] != "dyn-a" || got[1] != "dyn-b" {
		t.Fatalf("want [dyn-a dyn-b], got %v", got)
	}
	if again := fetch(); len(again) != 2 {
		t.Fatalf("cached list wrong, got %v", again)
	}
	if n := fu.callCount(); n != 1 {
		t.Fatalf("1h cache must serve the second call, got %d upstream calls %v", n, fu.calls())
	}
}
