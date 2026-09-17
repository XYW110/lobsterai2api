// checkin.go 每日签到：动态解析 clientVersion 后走
// GET slot → GET context → POST check_in（temp.txt 2026-09-17 实测流程），
// 每账号每日 +100 积分。无活动/已签到视为成功（记日志）；其余失败返回 error。
package upstream

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"lobsterai2api/internal/auth"
)

// updateAPIEnv 客户端版本发布接口地址的环境变量名。
// 本仓库不内置任何厂商域名（见 .trellis/spec/backend/upstream-integration.md），
// 未设置时跳过签到。
const updateAPIEnv = "LB2A_UPDATE_API"

// versionRe 校验 clientVersion 形如 1.2.3 / 1.2.3-beta.1。
var versionRe = regexp.MustCompile(`^\d+(\.\d+)*(-[0-9A-Za-z.-]+)?$`)

const versionCacheTTL = time.Hour

var versionCache struct {
	mu sync.Mutex
	v  string
	at time.Time
}

// resolveClientVersion 从更新接口解析客户端版本（缓存 versionCacheTTL）；
// 失败返回 error，调用方跳过本次签到。
func (c *Client) resolveClientVersion() (string, error) {
	versionCache.mu.Lock()
	defer versionCache.mu.Unlock()
	if versionCache.v != "" && time.Since(versionCache.at) < versionCacheTTL {
		return versionCache.v, nil
	}
	api := os.Getenv(updateAPIEnv)
	if api == "" {
		return "", fmt.Errorf("%s not set — version endpoint required for check-in", updateAPIEnv)
	}
	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var doc struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("update api parse: %w (body: %s)", err, truncate(string(raw), 120))
	}
	v := strings.TrimSpace(doc.Data.Value.Version)
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("update api returned bad version %q", doc.Data.Value.Version)
	}
	versionCache.v = v
	versionCache.at = time.Now()
	return v, nil
}

// DailyCheckin 为单个账号执行每日签到。
// 「无可用活动」「今日已签到」记日志后返回 nil；其余失败返回 error。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	ver, err := c.resolveClientVersion()
	if err != nil {
		return fmt.Errorf("resolve clientVersion: %w", err)
	}
	q := "placement=desktop_sidebar&clientVersion=" + url.QueryEscape(ver) +
		"&containerApiVersion=2&platform=win32"
	slotRaw, err := c.checkinJSON(a, ver, http.MethodGet, "/api/client-activities/slot?"+q, nil)
	if err != nil {
		return fmt.Errorf("slot: %w", err)
	}
	var slot struct {
		SlotState string `json:"slotState"`
		Activity  *struct {
			ActivityCode   string          `json:"activityCode"`
			ConfigRevision json.RawMessage `json:"configRevision"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(slotRaw, &slot); err != nil {
		return fmt.Errorf("slot parse: %w", err)
	}
	if slot.SlotState != "available" || slot.Activity == nil {
		log.Printf("checkin %s: no available activity (slotState=%q)", a.UID, slot.SlotState)
		return nil
	}
	if len(slot.Activity.ConfigRevision) == 0 {
		return fmt.Errorf("slot: activity %q missing configRevision", slot.Activity.ActivityCode)
	}
	code := url.PathEscape(slot.Activity.ActivityCode)
	rev := strings.Trim(string(slot.Activity.ConfigRevision), `"`)
	ctxRaw, err := c.checkinJSON(a, ver, http.MethodGet,
		"/api/client-activities/"+code+"/context?configRevision="+url.QueryEscape(rev), nil)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	var ctx struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
		} `json:"state"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(ctxRaw, &ctx); err != nil {
		return fmt.Errorf("context parse: %w", err)
	}
	if ctx.State.ClaimedToday {
		log.Printf("checkin %s: already claimed today", a.UID)
		return nil
	}
	hasCheckin := false
	for _, act := range ctx.Actions {
		if act == "check_in" {
			hasCheckin = true
			break
		}
	}
	if !hasCheckin {
		log.Printf("checkin %s: check_in action not offered", a.UID)
		return nil
	}
	payload := fmt.Sprintf(`{"configRevision":%s,"idempotencyKey":%q,"payload":{}}`,
		string(slot.Activity.ConfigRevision), newIdempotencyKey())
	resRaw, err := c.checkinJSON(a, ver, http.MethodPost,
		"/api/client-activities/"+code+"/actions/check_in", []byte(payload))
	if err != nil {
		return fmt.Errorf("check_in: %w", err)
	}
	var res struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(resRaw, &res); err != nil {
		return fmt.Errorf("check_in parse: %w", err)
	}
	gained := 0.0
	for _, k := range []string{"creditsGranted", "rewardCredits", "credits"} {
		if v, ok := res.Result[k].(float64); ok {
			gained = v
			break
		}
	}
	log.Printf("checkin %s: ok +%.0f credits", a.UID, gained)
	return nil
}

// checkinJSON 发签到链路请求（Bearer + LobsterAI/{ver} UA），解信封并校验 data 非空。
func (c *Client) checkinJSON(a *auth.Auth, ver, method, path string, body []byte) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ServerBase()+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LobsterAI/"+ver)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, fmt.Errorf("empty data (accessToken may be expired)")
	}
	return data, nil
}

// newIdempotencyKey 生成 UUIDv4（crypto/rand，go.mod 纯 stdlib）。
func newIdempotencyKey() string {
	var b [16]byte
	_, _ = cryptorand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
