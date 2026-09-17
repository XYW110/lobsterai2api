// Package upstream 封装对 LobsterAI 上游的全部 HTTP 调用。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"lobsterai2api/internal/auth"
)

const (
	clientVersion = "0.1.0"
	clientUA      = "LobsterAI/0.1.0"
)

// serverBaseOverride 由 SetServerBase 设置（config.json 里的 upstream.base_url）。
var serverBaseOverride string

// ServerBase returns the upstream API base URL.
// 取值优先级：SetServerBase > LB2A_UPSTREAM_BASE 环境变量。
// No hardcoded domain — users must set this in their config or environment.
func ServerBase() string {
	if serverBaseOverride != "" {
		return serverBaseOverride
	}
	if v := os.Getenv("LB2A_UPSTREAM_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
}

// SetServerBase 覆盖上游基址（来自 config.json 的 upstream.base_url）。
func SetServerBase(base string) {
	serverBaseOverride = strings.TrimRight(strings.TrimSpace(base), "/")
}

// apiEnvelope 上游统一信封（msg / message 两种字段名都出现过）。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// envMsg 取信封错误信息（msg 优先，message 兜底）。
func (e apiEnvelope) envMsg() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Message
}

// Client 上游 HTTP 客户端。
type Client struct {
	HTTP     *http.Client
	LastBody []byte // 最近一次非 2xx 响应体，供调用方 Classify
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP: &http.Client{Timeout: 180 * time.Second, Transport: tr},
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.LastBody = raw
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		msg := env.envMsg()
		kind := Classify(resp.StatusCode, msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(msg, 160))}
	}
	return env.Data, nil
}

// chatHeaders 设置 chat completions 请求头。
func chatHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-LobsterAI-Client-Capabilities", "kimi-k3-agentic-v1")
	req.Header.Set("X-LobsterAI-Client-Version", clientVersion)
}

// authHeaders 设置 auth 请求头（exchange/refresh 不需要 Bearer token）。
func authHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := ServerBase() + "/api/auth/refresh"
	body := a.KeyfromBody()
	body["refreshToken"] = a.RefreshToken
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	authHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	} else if exp := jwtExpiry(tok.AccessToken); exp > 0 {
		a.ExpiresAt = exp
	}
	return nil
}

// jwtExpiry 解码 JWT payload 的 exp（Unix 秒）；失败返回 0。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// prepareChatBody 预处理请求体：force stream=true（上游只支持流式，实测 stream:false 返回 500），
// 标准化 tool_choice。
func prepareChatBody(rawBody []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody // 解析失败原样发送
	}
	// force stream for upstream SSE compat
	body["stream"] = true
	// normalize tool_choice
	if tc, ok := body["tool_choice"]; ok {
		switch v := tc.(type) {
		case string:
			if v == "" || v == "none" {
				delete(body, "tool_choice")
			}
		case map[string]any:
			// object form, keep as-is
		case nil:
			delete(body, "tool_choice")
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return rawBody
	}
	return out
}

// peekBufSize 流首窥探窗口：足够容纳上游的 event:error 首帧。
const peekBufSize = 4 << 10

// peekCloser 先吐出窥探缓冲中的字节，再接底层 body，保证流首无损。
type peekCloser struct {
	br *bufio.Reader
	c  io.Closer
}

func (p *peekCloser) Read(b []byte) (int, error) { return p.br.Read(b) }
func (p *peekCloser) Close() error               { return p.c.Close() }

// isSSEErrorFrame 判定流首是否为上游藏在 HTTP 200 里的业务错误帧
// （典型：event:error + data:{"type":"error","error":{"code":40201,...}}）。
func isSSEErrorFrame(head []byte) bool {
	lower := strings.ToLower(string(head))
	if strings.Contains(lower, "event:error") || strings.Contains(lower, "event: error") {
		return true
	}
	return strings.Contains(lower, `"error":{`) || strings.Contains(lower, `"error": {`)
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、status 为上游状态码、err 为 nil（body 在 c.LastBody，
// 调用方用 Classify(status, body) 判定）；只有传输层失败才返回 err。
// HTTP 200 也会窥探流首（无损，同一 bufio.Reader 交给下游）：
// 命中 event:error / "error":{ 错误帧时按非 2xx 相同路径处置（rc=nil + LastBody）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, err error) {
	url := ServerBase() + "/api/proxy/v1/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepareChatBody(body)))
	if err != nil {
		return nil, 0, err
	}
	chatHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		c.LastBody = raw
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, nil
	}
	// 200 也可能携带业务错误帧（如 code=40201 额度用完），窥探流首识别。
	br := bufio.NewReaderSize(resp.Body, peekBufSize)
	head, perr := br.Peek(peekBufSize)
	if perr != nil && perr != io.EOF {
		resp.Body.Close()
		log.Printf("chat_stream uid=%s: peek error: %v", a.UID, perr)
		return nil, 0, perr
	}
	if isSSEErrorFrame(head) {
		raw, _ := io.ReadAll(io.LimitReader(br, 1<<20))
		resp.Body.Close()
		c.LastBody = raw
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream 200 error frame %s body=%s",
			a.UID, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, nil
	}
	return &peekCloser{br: br, c: resp.Body}, resp.StatusCode, nil
}

// FetchModels 调上游动态模型接口。
// GET {server}/api/models/available，Bearer accessToken。
// 返回模型 ID 列表；失败返回错误（调用方回退静态表）。
func (c *Client) FetchModels(a *auth.Auth) ([]string, error) {
	url := ServerBase() + "/api/models/available"
	body := a.KeyfromBody()
	// build query string from keyfrom
	parts := make([]string, 0)
	for k, v := range body {
		parts = append(parts, fmt.Sprintf("%s=%s", k, fmt.Sprintf("%v", v)))
	}
	if len(parts) > 0 {
		url += "?" + strings.Join(parts, "&")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data []struct {
			ModelID   string `json:"modelId"`
			ModelName string `json:"modelName"`
			Provider  string `json:"provider"`
			ApiFormat string `json:"apiFormat"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	ids := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return ids, nil
}

// QuotaUsage 查询账号当前积分。
// GET {server}/api/user/profile-summary 的 totalCreditsRemaining（含 free + campaign 活动积分）。
// 注意: /api/user/quota 只显示 freeCreditsTotal=300, 不含 5000 活动积分。
func (c *Client) QuotaUsage(a *auth.Auth) (remain int64, total int64, err error) {
	url := ServerBase() + "/api/user/profile-summary"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	var ps struct {
		TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
	}
	if err := json.Unmarshal(data, &ps); err != nil {
		return 0, 0, fmt.Errorf("profile-summary parse: %w", err)
	}
	clamp := func(v float64) int64 {
		if v < 0 {
			return 0
		}
		return int64(v)
	}
	if ps.TotalCreditsRemaining > 0 {
		return clamp(ps.TotalCreditsRemaining), 0, nil
	}
	return 0, 0, fmt.Errorf("profile-summary: no credits")
}
