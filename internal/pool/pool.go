// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"lobsterai2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Credits  int64     `json:"credits"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// rotateCredentials 用新凭证覆盖旧凭证，并在「旧 accessToken 非空且发生变化」时
// 视为重新登录：清空禁用/冷却/错误计数并返回 true，由调用方记日志。
//
// 必须排除空旧值：load() 塞入的占位账号 accessToken 为空，启动后 main 立刻 Add
// 真实凭证；不排除的话每次重启都会把持久化的 disabled 状态清掉。
// accessToken 而非 ExpiresAt/LatestKeyfrom 作指纹：它是 session 存活的直接凭据，
// 登录工具每次 exchange 都写新值；服务端自身 refresh 后 rescan 读回的字符串与内存
// 一致，不会误触发恢复。
func (e *entry) rotateCredentials(next *auth.Auth) bool {
	changed := e.a.AccessToken != "" && e.a.AccessToken != next.AccessToken
	e.a = next
	if !changed {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	return true
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]struct {
		Credits  int64     `json:"credits"`
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	} `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证（凭证变更即重登时自动恢复启用）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		if e.rotateCredentials(a) { // 重新登录 → 自动启用
			log.Printf("auth_rotate uid=%s: access token changed, account re-enabled", a.UID)
			// 必须立刻落盘：否则重启后 load 读回旧 disabled，而占位保护会阻止新凭证
			// 再次触发恢复，账号就永久停用（正是本功能要消除的故障）。
			p.saveLocked()
		}
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留），
// 凭证变更的账号自动恢复启用。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			if e.rotateCredentials(a) { // 重新登录 → 自动启用
				log.Printf("auth_rotate uid=%s: access token changed, account re-enabled", a.UID)
				p.saveLocked() // 同上：恢复必须持久，否则重启后回退成禁用
			}
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 禁用账号（session 死亡等需人工介入的情况）。恢复有两条路径：重新登录写入
// 新 accessToken（rotateCredentials 自动启用）或运维调用 Enable。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// Enable 手动清除账号的禁用与冷却状态（凭证未变化、无法自动判定时由运维调用）。
// 返回是否命中账号；credits 不受影响，未命中不写盘。
func (p *Pool) Enable(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	log.Printf("enable uid=%s: cleared disabled/cooling state", uid)
	p.saveLocked()
	return true
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		Credits:  e.credits,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:  s.Credits,
			disabled: s.Disabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Credits  int64     `json:"credits"`
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Credits  int64     `json:"credits"`
			Disabled bool      `json:"disabled"`
			Reason   string    `json:"reason,omitempty"`
			Until    time.Time `json:"until,omitempty"`
		}{
			Credits:  e.credits,
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
