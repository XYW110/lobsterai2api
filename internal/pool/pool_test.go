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

// addAuth 加入一个带 accessToken 的账号（空令牌会被 rotateCredentials 的占位保护特殊对待）。
func addAuth(p *Pool, uid, token string) *auth.Auth {
	a := &auth.Auth{UID: uid, AccessToken: token}
	p.Add(a)
	return a
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

// TestPickHighestCredits Pick 在 healthy 账号中取余额最高者，而非插入顺序。
func TestPickHighestCredits(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	addAuth(p, "u2", "tok-2")
	addAuth(p, "u3", "tok-3")
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 900)
	p.SetCredits("u3", 500)

	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("want highest-credit account u2, got %+v", got)
	}
	if got.AccessToken != "tok-2" {
		t.Fatalf("Pick must return the account's own credentials, got %+v", got)
	}
}

// TestPickSkipsCoolingAndDisabled 冷却中与禁用账号即使余额最高也必须跳过。
func TestPickSkipsCoolingAndDisabled(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	addAuth(p, "u2", "tok-2")
	addAuth(p, "u3", "tok-3")
	p.SetCredits("u1", 900)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 100)

	p.Disable("u1", "session dead")
	p.Cooldown("u2", CoolSoft, time.Hour, "429 rate limit")

	if st := statusOfUID(t, p, "u2"); !st.Cooling {
		t.Fatalf("u2 must report cooling, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("want only healthy account u3, got %+v", got)
	}
}

// TestPickExcludingSkipsTried PickExcluding 必须跳过请求内已试过的 uid（轮换语义）。
func TestPickExcludingSkipsTried(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	addAuth(p, "u2", "tok-2")
	p.SetCredits("u1", 900)
	p.SetCredits("u2", 500)

	if got := p.PickExcluding(map[string]bool{"u1": true}); got == nil || got.UID != "u2" {
		t.Fatalf("want next-best u2 after u1 tried, got %+v", got)
	}
	if got := p.PickExcluding(map[string]bool{"u1": true, "u2": true}); got != nil {
		t.Fatalf("all accounts tried must yield nil, got %+v", got)
	}
	// 空 map 等价于 Pick
	if got := p.PickExcluding(map[string]bool{}); got == nil || got.UID != "u1" {
		t.Fatalf("empty tried map must behave like Pick (u1), got %+v", got)
	}
}

// TestPickReturnsNilWhenEmpty 空池与「全部不可用」都返回 nil，调用方据此走 503。
func TestPickReturnsNilWhenEmpty(t *testing.T) {
	p, _ := newTestPool(t)
	if got := p.Pick(); got != nil {
		t.Fatalf("empty pool must return nil, got %+v", got)
	}

	addAuth(p, "u1", "tok-1")
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("all-disabled pool must return nil, got %+v", got)
	}
}

// TestCooldownExpiryRestoresHealth 冷却到期后恢复健康。用负时长构造「已过期」的
// until，避免 sleep（见 design.md 的确定性规则）。
func TestCooldownExpiryRestoresHealth(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")

	p.Cooldown("u1", CoolErr, time.Hour, "consecutive errors")
	if got := p.Pick(); got != nil {
		t.Fatalf("cooling account must not be picked, got %+v", got)
	}

	p.Cooldown("u1", CoolErr, -time.Second, "expired")
	st := statusOfUID(t, p, "u1")
	if st.Cooling {
		t.Fatalf("expired cooldown must not report cooling, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("expired cooldown must restore health, got %+v", got)
	}
}

// TestNoteErrorThresholdCoolDown 连续错误达阈值才冷却，且计数重置。
func TestNoteErrorThresholdCoolDown(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	const threshold = 3

	p.NoteError("u1", threshold, time.Hour)
	p.NoteError("u1", threshold, time.Hour)
	st := statusOfUID(t, p, "u1")
	if st.ErrCount != 2 || st.Cooling {
		t.Fatalf("below threshold: want errCount=2 cooling=false, got %+v", st)
	}

	p.NoteError("u1", threshold, time.Hour)
	st = statusOfUID(t, p, "u1")
	if !st.Cooling || st.ErrCount != 0 || st.Reason != "consecutive errors" {
		t.Fatalf("threshold reached: want cooling with errCount reset, got %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("threshold cooldown must block Pick, got %+v", got)
	}
}

// TestNoteSuccessResetsErrCount 成功请求必须清零连续错误计数（否则健康账号会被误冷却）。
func TestNoteSuccessResetsErrCount(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")

	p.NoteError("u1", 5, time.Hour)
	p.NoteError("u1", 5, time.Hour)
	if st := statusOfUID(t, p, "u1"); st.ErrCount != 2 {
		t.Fatalf("want errCount=2 before success, got %+v", st)
	}

	p.NoteSuccess("u1")
	if st := statusOfUID(t, p, "u1"); st.ErrCount != 0 {
		t.Fatalf("NoteSuccess must reset errCount, got %+v", st)
	}
}

// TestReenableIfCreditsSkipsDisabled 签到解冻只对「冷却且余额>0」生效：
// 禁用账号（需人工介入）不得借此恢复，余额<=0 也不解冻。
func TestReenableIfCreditsSkipsDisabled(t *testing.T) {
	p, _ := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	addAuth(p, "u2", "tok-2")

	// 分支 1：禁用账号即使签到拿到积分也不恢复，但 credits 会更新
	p.Disable("u1", "session dead")
	p.Cooldown("u1", CoolErr, time.Hour, "consecutive errors")
	p.ReenableIfCredits("u1", 100)
	st := statusOfUID(t, p, "u1")
	if !st.Disabled {
		t.Fatalf("disabled account must not unfreeze on credits, got %+v", st)
	}
	if st.Credits != 100 {
		t.Fatalf("credits must still be updated while disabled, got %d", st.Credits)
	}

	// 分支 2：冷却（非禁用）且余额>0 → 解冻
	p.Cooldown("u2", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u2", 500)
	st = statusOfUID(t, p, "u2")
	if st.Cooling || st.Credits != 500 || st.Reason != "" {
		t.Fatalf("cooling account with credits must unfreeze, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("unfrozen account must be pickable, got %+v", got)
	}

	// 分支 3：余额<=0 不解冻
	p.Cooldown("u2", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u2", 0)
	st = statusOfUID(t, p, "u2")
	if !st.Cooling || st.Credits != 0 {
		t.Fatalf("zero credits must not unfreeze, got %+v", st)
	}
}

// TestDisableSticky disabled 是终态：冷却到期或签到余额都不清除，只有 Enable /
// 凭证变更能解除（见 error-handling.md 的 disable / re-enable 生命周期）。
func TestDisableSticky(t *testing.T) {
	p, fp := newTestPool(t)
	addAuth(p, "u1", "tok-1")
	p.SetCredits("u1", 100)
	p.Disable("u1", "session dead")

	// 已过期的冷却不改变禁用态
	p.Cooldown("u1", CoolErr, -time.Second, "expired")
	// 签到余额>0 也不解除禁用
	p.ReenableIfCredits("u1", 999)

	st := statusOfUID(t, p, "u1")
	// 冷却已过期（Cooling=false），但禁用态不受影响
	if !st.Disabled || st.Cooling {
		t.Fatalf("disabled must be sticky across cooldown expiry, got %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled account must never be picked, got %+v", got)
	}

	if !p.Enable("u1") {
		t.Fatal("Enable must hit the account")
	}
	if st := statusOfUID(t, p, "u1"); st.Disabled {
		t.Fatalf("Enable must clear disabled, got %+v", st)
	}
	if s := readState(t, fp).Accounts["u1"]; s.Disabled {
		t.Fatalf("Enable must persist, got %+v", s)
	}
}

// TestListSortedByUID List 输出按 uid 稳定排序（/status 与 admin enable 都依赖它）。
func TestListSortedByUID(t *testing.T) {
	p, _ := newTestPool(t)
	for _, uid := range []string{"u3", "u1", "u2"} {
		addAuth(p, uid, "tok-"+uid)
	}
	p.SetCredits("u2", 42)

	got := p.List()
	want := []string{"u1", "u2", "u3"}
	if len(got) != len(want) {
		t.Fatalf("want %d accounts, got %d: %+v", len(want), len(got), got)
	}
	for i, uid := range want {
		if got[i].UID != uid {
			t.Fatalf("List()[%d].UID: want %q, got %q (full=%+v)", i, uid, got[i].UID, got)
		}
	}
	if got[1].Credits != 42 {
		t.Fatalf("credits must be reported per account, got %+v", got[1])
	}
}

// TestStateRoundTrip state.json 往返保留 credits/disabled/until；load 的占位账号
// （空令牌）被 Add 换成真实凭证后状态不回退。原子写不得留 .tmp。
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1", Nickname: "One"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "tok-2"})
	p.SetCredits("u1", 777)
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Disable("u2", "session dead")

	before := statusOfUID(t, p, "u1")
	// 原子写：saveLocked 走 tmp + rename，不得残留临时文件
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("state writes must not leave tmp files, got %v", leftovers)
	}

	// 模拟重启
	p2 := New(fp)
	st1 := statusOfUID(t, p2, "u1")
	if !st1.Cooling || st1.Credits != 777 || st1.Reason != "余额不足" {
		t.Fatalf("credits/cooling/reason must survive restart, got %+v", st1)
	}
	if !st1.Until.Equal(before.Until) {
		t.Fatalf("until must survive round trip: want %v, got %v", before.Until, st1.Until)
	}
	st2 := statusOfUID(t, p2, "u2")
	if !st2.Disabled || st2.Reason != "session dead" {
		t.Fatalf("disabled/reason must survive restart, got %+v", st2)
	}

	// load() 塞入的是空令牌占位账号；Add 真实凭证替换它
	if a := p2.AuthByUID("u1"); a == nil || a.AccessToken != "" {
		t.Fatalf("load must seed an empty-token placeholder, got %+v", a)
	}
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1", Nickname: "One"})
	a := p2.AuthByUID("u1")
	if a == nil || a.AccessToken != "tok-1" || a.Nickname != "One" {
		t.Fatalf("Add must replace the placeholder with real credentials, got %+v", a)
	}
	// 占位替换不算凭证变更：冷却状态必须保留
	if st := statusOfUID(t, p2, "u1"); !st.Cooling || st.Credits != 777 {
		t.Fatalf("placeholder replacement must keep state, got %+v", st)
	}
}

// TestSyncToDirAddsAndRemoves SyncToDir 对齐扫描结果：新账号加入、消失的剔除，
// 令牌未变的既有账号状态保留。
func TestSyncToDirAddsAndRemoves(t *testing.T) {
	p, _ := newTestPool(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "tok-2"})
	p.SetCredits("u2", 321)

	p.SyncToDir([]*auth.Auth{
		{UID: "u2", AccessToken: "tok-2"},
		{UID: "u3", AccessToken: "tok-3"},
	})

	if a := p.AuthByUID("u1"); a != nil {
		t.Fatalf("u1 disappeared from disk and must be removed, got %+v", a)
	}
	if st := statusOfUID(t, p, "u2"); st.Credits != 321 {
		t.Fatalf("u2 state must survive sync, got %+v", st)
	}
	if st := statusOfUID(t, p, "u3"); st.Credits != 0 || st.Disabled {
		t.Fatalf("u3 must be added fresh, got %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("only u2 has credits and must be picked, got %+v", got)
	}
}

// assertStateMatchesPool 断言 state.json 的账号集合与字段和内存完全一致（双向：数量相等，
// 且每个内存账号在盘上存在、credits/disabled/reason/until 一致）。
func assertStateMatchesPool(t *testing.T, p *Pool, fp string) {
	t.Helper()
	onDisk := readState(t, fp).Accounts
	mem := p.List()
	if len(onDisk) != len(mem) {
		t.Fatalf("state.json has %d accounts, pool has %d: disk=%+v mem=%+v",
			len(onDisk), len(mem), onDisk, mem)
	}
	for _, st := range mem {
		s, ok := onDisk[st.UID]
		if !ok {
			t.Fatalf("uid %s missing from state.json (disk=%+v)", st.UID, onDisk)
		}
		if s.Credits != st.Credits || s.Disabled != st.Disabled || s.Reason != st.Reason || !s.Until.Equal(st.Until) {
			t.Fatalf("uid %s mismatch: disk=%+v mem=%+v", st.UID, s, st)
		}
	}
}

// TestSyncToDirPersistsAddAndRemove 重扫的增删必须落盘：否则 rescan 移除账号后
// state.json 仍留着它，若在下一次其它变更前重启，load() 会把它复活成 accessToken
// 为空的占位账号，Pick() 选中后向上游发空 Bearer（R-T9 要消除的故障）。
// 无变化的重扫则不得写盘（30s 一次的重扫不能变成写放大）。
func TestSyncToDirPersistsAddAndRemove(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1", Nickname: "One"})
	p.SetCredits("u1", 100)

	// 新增账号（rescan 发现新 auth 文件）→ 落盘
	p.SyncToDir([]*auth.Auth{
		{UID: "u1", AccessToken: "tok-1"},
		{UID: "u2", AccessToken: "tok-2"},
	})
	if _, ok := readState(t, fp).Accounts["u2"]; !ok {
		t.Fatalf("added account must be persisted, got %+v", readState(t, fp).Accounts)
	}
	assertStateMatchesPool(t, p, fp)

	// 移除账号（auth 文件被删除）→ 落盘
	p.SyncToDir([]*auth.Auth{{UID: "u2", AccessToken: "tok-2"}})
	if a := p.AuthByUID("u1"); a != nil {
		t.Fatalf("u1 must be removed from memory, got %+v", a)
	}
	assertStateMatchesPool(t, p, fp)
	if _, ok := readState(t, fp).Accounts["u1"]; ok {
		t.Fatalf("removed account must not remain in state.json, got %+v", readState(t, fp).Accounts)
	}

	// 模拟重启：被移除的 uid 不得复活成占位账号，新增的 uid 仍在
	p2 := New(fp)
	if a := p2.AuthByUID("u1"); a != nil {
		t.Fatalf("removed account must not be resurrected on reload, got %+v", a)
	}
	for _, st := range p2.List() {
		if st.UID == "u1" {
			t.Fatalf("removed account must not appear after reload, got %+v", st)
		}
	}
	if a := p2.AuthByUID("u2"); a == nil {
		t.Fatal("added account must survive reload")
	}

	// 无变化的重扫不得写盘：先删掉 state.json，同一集合重扫后文件不应重新出现
	//（load 的占位账号 accessToken 为空，与扫描结果一致 → 不构成凭证变更）
	if err := os.Remove(fp); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	p2.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if _, err := os.Stat(fp); err == nil {
		t.Fatal("unchanged rescan must not rewrite state.json")
	}
}
