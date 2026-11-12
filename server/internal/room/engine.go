// Package room 是会议的领域引擎：状态机、发言权、方案、投票、概述、结论聚合。
//
// v2 状态机（docs/07-会议流程规格v2.md §四）：
//
//	briefing ──► debating ──► voting ──► summarizing ──► ended
//
// 所有产物（结论 / 概述兜底 / ownerReport / 立场矩阵）都是**机械聚合**，零模型调用。
package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"agchat/internal/budget"
	"agchat/internal/domain"
	"agchat/internal/store"
)

// 规格 §二 / §十 的硬上限与默认值。
const (
	DefTokenBudget       = 20000000 // 2000w
	DefMaxMinutes        = 30
	DefMaxRounds         = 10
	CapMaxRounds         = 10 // 轮数硬上限
	DefMaxOutput         = 800
	DefMaxAgents         = 6
	CapMaxAgents         = 6
	MaxProposalsPerAgent = 2
	// MaxRevisionsPerProposal 限制单个方案的修订次数：修订不占方案配额，
	// 但必须有个上限，否则一个 agent 可以用"改到自己满意"为名无限刷写入。
	MaxRevisionsPerProposal = 2

	DefBriefing     = 120 * time.Second
	MaxWaitSeconds  = 20
	MaxReadMessages = 60

	// 链式超时：连续这么多次跳过 → 见 tick 里的 round_idle（按轮计时，不是按人次）
	ChainTimeoutLimit = 6

	// 连续空轮（整轮零发言）达到这个数就直接进投票，防无限空转
	EmptyRoundsToVoting = 3

	// 投票截止：全员投完立即结束，否则这么久后锁定（规格没给时长，Lead 已裁决）
	VotingWindow = 60 * time.Second

	// DefVoteGrace 是"全员投完后额外等一小段"的默认值，给被误判掉线的人回来补投的机会。
	// 不能用 1×turn-timeout：生产 90s，等于把刚修好的秒级结算又还回去。
	DefVoteGrace = 10 * time.Second

	alphabet          = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	readBillingStopAt = 0.99
)

// Defaults 是建会时的默认值（可由 flag 覆盖，见 §十）。
type Defaults struct {
	TokenBudget     int
	MaxMinutes      int
	MaxRounds       int
	MaxOutputTokens int
	MaxAgents       int
}

// DefaultDefaults 返回规格 §二的默认值。
func DefaultDefaults() Defaults {
	return Defaults{
		TokenBudget: DefTokenBudget, MaxMinutes: DefMaxMinutes,
		MaxRounds: DefMaxRounds, MaxOutputTokens: DefMaxOutput,
		MaxAgents: DefMaxAgents,
	}
}

// SetDefaults 覆盖建会默认值。
func (e *Engine) SetDefaults(d Defaults) {
	if d.TokenBudget > 0 {
		e.def.TokenBudget = d.TokenBudget
	}
	if d.MaxMinutes > 0 {
		e.def.MaxMinutes = d.MaxMinutes
	}
	if d.MaxRounds > 0 {
		e.def.MaxRounds = d.MaxRounds
	}
	if d.MaxOutputTokens > 0 {
		e.def.MaxOutputTokens = d.MaxOutputTokens
	}
	if d.MaxAgents > 0 {
		e.def.MaxAgents = d.MaxAgents
	}
}

// Engine 是会议服务的全部逻辑。
type Engine struct {
	store     *store.Store
	turnTO    time.Duration
	briefing  time.Duration
	summaryTO time.Duration
	voteGrace time.Duration
	nowFn     func() int64
	def       Defaults
	// presenceAbstain 控制"掉线者预置弃权"这条判定（arch R-2）。
	// 默认 true（agchatd 行为逐字节不变）；本地宿主设 false，因为它的输入
	// LastSeenAt 不在 op 流里，开着会让重放不可复算（docs/09 §5）。
	presenceAbstain bool

	mu sync.Mutex
	rt map[string]*runtime
}

type runtime struct {
	mu        sync.Mutex
	tenantID  string
	event     int64
	waiters   map[chan struct{}]struct{}
	lastTouch int64
}

// New 造一个引擎。
func New(st *store.Store, turnTimeout, briefing, summaryTimeout time.Duration) *Engine {
	if turnTimeout <= 0 {
		turnTimeout = 90 * time.Second
	}
	if briefing <= 0 {
		briefing = DefBriefing
	}
	if summaryTimeout <= 0 {
		summaryTimeout = 60 * time.Second
	}
	return &Engine{
		store: st, turnTO: turnTimeout, briefing: briefing, summaryTO: summaryTimeout,
		nowFn: domain.NowMillis, rt: map[string]*runtime{}, def: DefaultDefaults(),
		voteGrace: DefVoteGrace, presenceAbstain: true,
	}
}

// SetPresenceAbstain 开关"掉线者预置弃权"（arch R-2）。
//
// 默认 true —— agchatd 的行为因此逐字节不变。本地宿主显式设 false：
// presetAbstainForGone 的判据是 LastSeenAt（轮询到达时刻），它不是 op，
// 记不进日志；关掉之后"谁会弃权"只由投票窗口截止这一条可被记录的 advance 决定，
// 重放才可复算（docs/09 §5）。
func (e *Engine) SetPresenceAbstain(enabled bool) { e.presenceAbstain = enabled }

// SetVoteGrace 设置投票宽限期（0 = 关闭，全员投完立即结算）。
func (e *Engine) SetVoteGrace(d time.Duration) {
	if d < 0 {
		d = 0
	}
	e.voteGrace = d
}

// SetClock 给测试注入时间。
func (e *Engine) SetClock(fn func() int64) { e.nowFn = fn }

func (e *Engine) now() int64 { return e.nowFn() }

func (e *Engine) runtimeFor(tenantID, roomID string) *runtime {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok := e.rt[roomID]
	if !ok {
		rt = &runtime{waiters: map[chan struct{}]struct{}{}}
		e.rt[roomID] = rt
	}
	if tenantID != "" {
		rt.tenantID = tenantID
	}
	rt.lastTouch = e.now()
	return rt
}

func (rt *runtime) broadcast() {
	rt.event++
	for ch := range rt.waiters {
		// 唤醒订阅者时兜 panic：这里若崩，整个进程就没了，会议里的 agent 全部失联
		func(c chan struct{}) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("waiter_panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				}
			}()
			select {
			case c <- struct{}{}:
			default:
			}
		}(ch)
	}
	rt.waiters = map[chan struct{}]struct{}{}
}

// SafeGo 起一个带 panic 兜底的 goroutine。
//
// ★ 为什么必须有：net/http 只 recover **请求协程**。后台协程里一次 panic 会直接
//
//	干掉整个进程 —— 会议开到一半服务没了，所有 agent 的 token 全部失效、拿不回结果。
//	这比任何功能缺陷都严重，所以每个后台入口都要兜住。
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background_panic",
					"where", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// safeCall 在同步回调里兜 panic（例如长轮询唤醒时逐个通知订阅者）。
func safeCall(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("callback_panic",
				"where", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// RunTicker 后台推进状态：briefing 到期、回合超时、投票截止、概述超时。
func (e *Engine) RunTicker(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// 单次 tick 出错不能掀掉整个 ticker
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("ticker_panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					}
				}()
				e.tickAll()
			}()
		}
	}
}

func (e *Engine) tickAll() {
	type ref struct{ tenant, room string }
	e.mu.Lock()
	refs := make([]ref, 0, len(e.rt))
	for id, rt := range e.rt {
		if rt.tenantID != "" {
			refs = append(refs, ref{rt.tenantID, id})
		}
	}
	e.mu.Unlock()
	for _, r := range refs {
		// 单个房间 panic 不能拖垮其它房间
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("room_tick_panic", "room", r.room, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				}
			}()
			_ = e.Do(context.Background(), r.tenant, r.room, func(agg *Agg, tx *store.Tx) (bool, error) {
				return e.tick(agg), nil
			})
		}()
	}
}

// =============================================================================
// 聚合
// =============================================================================

// Agg 是一个房间的完整快照。
type Agg struct {
	Room      *domain.Room
	Agents    []*domain.Agent
	Messages  []*domain.Message
	Proposals []*domain.Proposal
	Votes     []*domain.Vote

	pendingMsgs []*domain.Message
	dirtyProps  map[string]bool
	dirtyVotes  map[string]bool
}

func (a *Agg) AgentByID(id string) *domain.Agent {
	for _, ag := range a.Agents {
		if ag.ID == id {
			return ag
		}
	}
	return nil
}

func (a *Agg) AgentByToken(hash string) *domain.Agent {
	for _, ag := range a.Agents {
		if ag.TokenHash == hash {
			return ag
		}
	}
	return nil
}

// Moderator 返回主席。
func (a *Agg) Moderator() *domain.Agent {
	if a.Room.ModeratorID == "" {
		return nil
	}
	return a.AgentByID(a.Room.ModeratorID)
}

func (a *Agg) ProposalByID(id string) *domain.Proposal {
	for _, p := range a.Proposals {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (a *Agg) VoteByAgent(agentID string) *domain.Vote {
	for _, v := range a.Votes {
		if v.AgentID == agentID {
			return v
		}
	}
	return nil
}

// ProposalsBy 返回某人提交的**独立方案数**（修订版不计入配额）。
func (a *Agg) ProposalsBy(agentID string) int {
	n := 0
	for _, p := range a.Proposals {
		if p.AgentID == agentID && p.Supersedes == "" {
			n++
		}
	}
	return n
}

// RootProposalID 沿着 supersedes 往回找到链首。
func (a *Agg) RootProposalID(id string) string {
	cur := a.ProposalByID(id)
	for cur != nil && cur.Supersedes != "" {
		parent := a.ProposalByID(cur.Supersedes)
		if parent == nil {
			break
		}
		cur = parent
	}
	if cur == nil {
		return id
	}
	return cur.ID
}

// ChainRevisions 返回整条修订链上一共有几个修订版（不含链首本身）。
// 修订上限按**链**算，不按单节点算 —— 否则一串"修订自己刚改出来的版本"就能绕过上限。
func (a *Agg) ChainRevisions(id string) int {
	root := a.RootProposalID(id)
	n := 0
	for _, p := range a.Proposals {
		if p.Supersedes != "" && a.RootProposalID(p.ID) == root && p.ID != root {
			n++
		}
	}
	return n
}

// touchProposal 把方案标记为"需要落库"，但不重复加入列表。
func (a *Agg) touchProposal(id string) {
	if a.dirtyProps == nil {
		a.dirtyProps = map[string]bool{}
	}
	a.dirtyProps[id] = true
}

// AdoptedProposals 返回"当前采纳中"的方案（没有被更新的版本取代）。
func (a *Agg) AdoptedProposals() []*domain.Proposal {
	out := make([]*domain.Proposal, 0, len(a.Proposals))
	for _, p := range a.Proposals {
		if p.SupersededBy == "" {
			out = append(out, p)
		}
	}
	return out
}

// LatestAdopted 沿着修订链找到当前采纳的版本（用于"投给了已被取代的方案"时自动改投）。
func (a *Agg) LatestAdopted(id string) *domain.Proposal {
	p := a.ProposalByID(id)
	for p != nil && p.SupersededBy != "" {
		p = a.ProposalByID(p.SupersededBy)
	}
	return p
}

func (a *Agg) pushMsg(m *domain.Message) {
	a.Messages = append(a.Messages, m)
	a.pendingMsgs = append(a.pendingMsgs, m)
}

func (a *Agg) addProposal(p *domain.Proposal) {
	a.Proposals = append(a.Proposals, p)
	if a.dirtyProps == nil {
		a.dirtyProps = map[string]bool{}
	}
	a.dirtyProps[p.ID] = true
}

func (a *Agg) putVote(v *domain.Vote) {
	replaced := false
	for i, old := range a.Votes {
		if old.AgentID == v.AgentID {
			a.Votes[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		a.Votes = append(a.Votes, v)
	}
	if a.dirtyVotes == nil {
		a.dirtyVotes = map[string]bool{}
	}
	a.dirtyVotes[v.AgentID] = true
}

func loadAgg(tx *store.Tx, roomID string) (*Agg, error) {
	r, err := tx.GetRoom(roomID)
	if err != nil {
		return nil, err
	}
	agents, err := tx.ListAgents(roomID)
	if err != nil {
		return nil, err
	}
	msgs, err := tx.ListMessages(roomID, 0)
	if err != nil {
		return nil, err
	}
	props, err := tx.ListProposals(roomID)
	if err != nil {
		return nil, err
	}
	votes, err := tx.ListVotes(roomID)
	if err != nil {
		return nil, err
	}
	return &Agg{Room: r, Agents: agents, Messages: msgs, Proposals: props, Votes: votes}, nil
}

func saveAgg(tx *store.Tx, a *Agg) error {
	if err := tx.SaveRoom(a.Room); err != nil {
		return err
	}
	for _, ag := range a.Agents {
		if err := tx.SaveAgent(ag); err != nil {
			return err
		}
	}
	for _, m := range a.pendingMsgs {
		if err := tx.InsertMessage(m); err != nil {
			return err
		}
	}
	for _, p := range a.Proposals {
		if a.dirtyProps[p.ID] {
			if err := tx.UpsertProposal(p); err != nil {
				return err
			}
		}
	}
	for _, v := range a.Votes {
		if a.dirtyVotes[v.AgentID] {
			if err := tx.UpsertVote(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Do 是所有操作的统一入口：拿房间锁 → 开租户事务 → 载入聚合 → 跑 fn → 落库 → 广播。
func (e *Engine) Do(ctx context.Context, tenantID, roomID string,
	fn func(agg *Agg, tx *store.Tx) (bool, error)) error {

	rt := e.runtimeFor(tenantID, roomID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return e.runLocked(ctx, tenantID, roomID, rt, fn)
}

func (e *Engine) runLocked(ctx context.Context, tenantID, roomID string, rt *runtime,
	fn func(agg *Agg, tx *store.Tx) (bool, error)) error {

	var bizErr error
	changed := false

	err := e.store.WithTenant(ctx, tenantID, func(tx *store.Tx) error {
		agg, err := loadAgg(tx, roomID)
		if err != nil {
			return err
		}
		c, fnErr := fn(agg, tx)
		changed = c
		if c {
			if err := saveAgg(tx, agg); err != nil {
				return err
			}
		}
		if fnErr != nil {
			if _, ok := fnErr.(*domain.Error); ok {
				bizErr = fnErr
				return nil
			}
			return fnErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	rt.lastTouch = e.now()
	if changed {
		rt.broadcast()
	}
	return bizErr
}

// =============================================================================
// 状态机
// =============================================================================

func (e *Engine) pct(r *domain.Room) float64 {
	if r.TokenBudget <= 0 {
		return 0
	}
	return float64(r.BudgetUsed) / float64(r.TokenBudget)
}

// TurnOrder 是发言次序：主席第 1，其余按进入顺序。
func turnOrder(agg *Agg) []string {
	out := make([]string, 0, len(agg.Agents))
	if agg.Room.ModeratorID != "" && agg.AgentByID(agg.Room.ModeratorID) != nil {
		out = append(out, agg.Room.ModeratorID)
	}
	for _, a := range agg.Agents {
		if a.ID != agg.Room.ModeratorID {
			out = append(out, a.ID)
		}
	}
	return out
}

func currentTurnAgent(agg *Agg) *domain.Agent {
	r := agg.Room
	if r.State != domain.StateDebating {
		return nil
	}
	if r.Plan == nil || r.Plan.Pos >= len(r.Plan.Queue) {
		return nil
	}
	return agg.AgentByID(r.Plan.Queue[r.Plan.Pos])
}

func (e *Engine) buildPlan(agg *Agg) {
	r := agg.Room
	queue := turnOrder(agg)
	r.Plan = &domain.Plan{Round: r.Round, Queue: queue, Pos: 0, TurnStartedAt: e.now()}
	r.RoundIdleSince = e.now() // 本轮开始计时
}

// startDebate 进入讨论（§八 #2：只有 1 人时跳过辩论，直接写概述）。
func (e *Engine) startDebate(agg *Agg) {
	r := agg.Room
	if len(agg.Agents) == 0 {
		return
	}
	r.StartedAt = e.now()
	if len(agg.Agents) == 1 {
		// 只有 1 人：不辩论、不投票，直接让它出概述
		r.State = domain.StateSummarizing
		r.PhaseStartedAt = e.now()
		r.EndReason = "solo_agent"
		return
	}
	r.State = domain.StateDebating
	r.Round = 1
	r.ChainTimeouts = 0
	e.buildPlan(agg)
}

// enterVoting 进入投票（原因写进 convergence_reason）。
func (e *Engine) enterVoting(agg *Agg, reason string) {
	r := agg.Room
	if r.State == domain.StateVoting || r.State == domain.StateSummarizing || r.State == domain.StateEnded {
		return
	}
	r.State = domain.StateVoting
	r.ConvergenceReason = reason
	r.PhaseStartedAt = e.now()
	r.Plan = nil
}

// enterSummarizing 进入写概述。winner 为空表示没有方案（机械摘要兜底）。
func (e *Engine) enterSummarizing(agg *Agg) {
	r := agg.Room
	if r.State == domain.StateSummarizing || r.State == domain.StateEnded {
		return
	}
	if winner := TallyWinner(agg); winner != nil {
		r.WinnerProposalID = winner.ID
	} else {
		r.WinnerProposalID = ""
	}
	r.State = domain.StateSummarizing
	r.PhaseStartedAt = e.now()
	r.Plan = nil
}

// 为什么按"轮"而不是按"人次"：6 人 × 90s = 9 分钟才跳轮，而一场会总共才 30 分钟，
// 等于把会议耗死在空等上。按轮计时后，6 人满场最坏情况从 9 分钟降到 90 秒。
// roundExhausted 本轮所有发言位都用完了（说满了，或者全被跳过）——这里才决定"进下一轮"还是"空轮"。
//
// ★ 关键区别（这是曾经把真实会议卡死的 bug）：
//   - "某个 agent 超时" ≠ "整轮没人说话"。
//     前者只跳过那一个人、把发言权推进到下一位；本轮继续，绝不能重置回第一位，
//     否则只要主席不说话，其余人就永远轮不到，最后被连续空轮直接拖进投票。
//   - 只有当**本轮所有人都被给过机会、且一条发言都没有**，才算空轮。
func (e *Engine) roundExhausted(agg *Agg) {
	r := agg.Room
	if r.State != domain.StateDebating {
		return
	}

	spoke := false
	for _, m := range agg.Messages {
		if m.Round == r.Round {
			spoke = true
			break
		}
	}

	if !spoke {
		// 整轮零发言：不推进 round（白烧一轮是纯损失），同一轮重开；
		// 连续 EmptyRoundsToVoting 个空轮才进投票，防无限空转。
		r.EmptyRounds++
		r.RoundSkipped = true
		if r.EmptyRounds >= EmptyRoundsToVoting {
			e.enterVoting(agg, "empty_rounds")
			return
		}
		e.buildPlan(agg)
		return
	}

	r.EmptyRounds = 0
	r.RoundSkipped = false
	if r.Round >= r.MaxRounds {
		e.enterVoting(agg, "rounds_done")
		return
	}
	r.Round++
	e.buildPlan(agg)
}

func (e *Engine) advancePlan(agg *Agg) {
	r := agg.Room
	r.Plan.Pos++
	r.Plan.TurnStartedAt = e.now()
	if r.Plan.Pos >= len(r.Plan.Queue) {
		e.roundExhausted(agg)
	}
}

func (e *Engine) skipIdleTurn(agg *Agg) bool {
	r := agg.Room
	p := r.Plan
	if p == nil || len(p.Queue) == 0 || p.Pos >= len(p.Queue) {
		return false
	}
	if e.now()-p.TurnStartedAt < e.turnTO.Milliseconds() {
		return false
	}
	p.Skipped = append(p.Skipped, p.Queue[p.Pos])
	r.ChainTimeouts++ // 仅诊断；跳轮判据见 tick 里的 round_idle
	e.advancePlan(agg)
	return true
}

// tick 推进状态。返回是否有变化。
// maybeSettleVoting 判断投票是否该结算，并在该结算时推进状态。
//
// ★ 只有这一个判定入口：tick 和 Vote 都必须走它。
//
//	曾经 Vote 里另写了一份"全员投完立即结算"，结果宽限期被完全绕过 ——
//	预置弃权一落地就 ended，掉线的人回来只能吃到 403。
func (e *Engine) maybeSettleVoting(agg *Agg) bool {
	r := agg.Room
	if r.State != domain.StateVoting {
		return false
	}
	// 投票窗口固定 60s，从进入 voting 起算（规格 §五 裁决 #3）。
	//
	// 为什么**不**像概述那样按"未投票者的活动"续期：agent 是靠长轮询被唤醒的
	// （状态一变就唤醒，不用等 wait 超时），它拿到 voting 的那一刻就是窗口的起点；
	// 投一票只是发一个 proposalId，不需要思考时间。给它续期只会把最坏情况从 60s
	// 拖到 120s，而不会救回任何一个真的会投票的 agent。
	// 救不回来的那种情况（人早走了 / 就是不发）由"截止即计弃权"兜住（见 settleVoting）。
	deadlineHit := e.now()-r.PhaseStartedAt >= VotingWindow.Milliseconds()
	allVoted := len(agg.Votes) >= len(agg.Agents)
	return e.settleVoting(agg, deadlineHit, allVoted)
}

// settleVoting 是投票结算的唯一出口：判定是否该结算、该结算时把没投的人计为弃权、然后收口。
//
// ★ 为什么"没投的人"必须被计成弃权票，而不是干脆没有票：
//
//	docs/07 §十五 有一条不变量——`voteTotals` 各项之和 == 实际投票人数。
//	实测（Lead 亲自当 agent）3 人参会、2 人投票，votes 之和是 2，而 abstain 是 0：
//	分母错了。任何基于这个分母的"多数/分裂"判断都不可信，而页面上的"未投票"只是
//	一句文案，机器读到的字段仍然少一个人。
func (e *Engine) settleVoting(agg *Agg, deadlineHit, allVoted bool) bool {
	r := agg.Room
	settle := deadlineHit
	if allVoted && !settle {
		// 宽限期只为"可能有人会回来"而存在：
		// 只有存在系统预置的弃权票时才等，否则全员自愿投完就该立刻结算
		// —— 否则每场会都平白多等 10 秒，把刚修好的秒级结算又还回去。
		hasPreset := false
		for _, v := range agg.Votes {
			if v.Reason == AutoAbstainReason || v.Reason == VoteWindowAbstainReason {
				hasPreset = true
				break
			}
		}
		switch {
		case !hasPreset || e.voteGrace <= 0:
			settle = true
		default:
			var last int64
			for _, v := range agg.Votes {
				if v.CreatedAt > last {
					last = v.CreatedAt
				}
			}
			settle = e.now()-last >= e.voteGrace.Milliseconds()
		}
	}
	if !settle {
		return false
	}
	// 截止时仍未投票的人 → 计为弃权，保证 voteTotals 之和 == 参会人数
	for _, a := range agg.Agents {
		if agg.VoteByAgent(a.ID) != nil {
			continue
		}
		agg.putVote(&domain.Vote{
			TenantID: r.TenantID, RoomID: r.ID, AgentID: a.ID,
			Reason: VoteWindowAbstainReason, CreatedAt: e.now(),
		})
	}
	if TallyWinner(agg) == nil {
		e.endRoom(agg, "no_winner")
	} else {
		e.enterSummarizing(agg)
	}
	return true
}

// GoneAfterTurns 是"这个人算掉线"的判据倍数：这么久没有任何请求就当它不会再来了。
// 生产默认 turn-timeout=90s → 270s，比 20s 长轮询宽一个量级，不会误判正常心跳。
const GoneAfterTurns = 3

// AutoAbstainReason 标记"这是系统替掉线者预置的弃权"，用于区分并允许被本人撤销。
const AutoAbstainReason = "已掉线，自动弃权"

// VoteWindowAbstainReason 标记"投票截止时还没投，计为弃权"。
// 与 AutoAbstainReason 分开：两者都算弃权，但成因不同（一个掉线、一个只是慢），
// 而且它出现在窗口已经 locked 之后，本人已经没有机会再改票。
const VoteWindowAbstainReason = "投票截止未投票，计为弃权"

// presetAbstainForGone 给掉线的人预置弃权票。
//
// 为什么不能等投票窗口自然到期：还活着的人要白等 60 秒，等一个永远不会来的 agent。
// 为什么必须可撤销：阈值永远只能靠猜，把"误判"的代价从"永久错误"降成"本人能纠正"，
// 比把阈值调准更可靠 —— 被预置的人只要在投票截止前回来投票，就覆盖掉这条。
func (e *Engine) presetAbstainForGone(agg *Agg) bool {
	if !e.presenceAbstain {
		// 本地宿主：这条判定的输入不在 op 流里，关掉它（arch R-2 / docs/09 §5）。
		// 剩下的弃权来源只有 settleVoting 的"投票窗口截止"，那会落 advance op。
		return false
	}
	r := agg.Room
	if r.State != domain.StateVoting {
		return false
	}
	limit := GoneAfterTurns * e.turnTO.Milliseconds()
	changed := false
	for _, a := range agg.Agents {
		if agg.VoteByAgent(a.ID) != nil {
			continue
		}
		if a.LastSeenAt == 0 || e.now()-a.LastSeenAt <= limit {
			continue
		}
		agg.putVote(&domain.Vote{
			TenantID: r.TenantID, RoomID: r.ID, AgentID: a.ID,
			Reason: AutoAbstainReason, CreatedAt: e.now(),
		})
		changed = true
	}
	return changed
}

func (e *Engine) tick(agg *Agg) bool {
	r := agg.Room
	if r.State == domain.StateEnded {
		return false
	}
	changed := false

	// 墙钟到点：同预算耗尽，立刻进投票（§八 #7）
	minutes := int64(r.MaxMinutes)
	if minutes > 0 && e.now()-r.CreatedAt >= minutes*60000 && r.State == domain.StateDebating {
		hasProposals := len(agg.Proposals) > 0
		if hasProposals {
			e.enterVoting(agg, "time_limit")
		} else {
			e.endRoom(agg, "time_limit")
		}
		return true
	}

	switch r.State {
	case domain.StateBriefing:
		if e.now() >= r.BriefingDeadline || len(agg.Agents) >= r.MaxAgents {
			if len(agg.Agents) == 0 {
				e.endRoom(agg, "no_participants") // §八 #1 无人参会
				return true
			}
			e.startDebate(agg)
			changed = true
		}

	case domain.StateDebating:
		// 预算耗尽 → 立刻转投票（§八 #5）
		if e.pct(r)*100 >= budget.HardPct {
			e.enterVoting(agg, "budget")
			changed = true
			break
		}
		if e.skipIdleTurn(agg) {
			changed = true
		}

	case domain.StateVoting:
		// 掉线的人先预置成弃权：否则活着的人要白等满整个投票窗口，
		// 等一个永远不会来的人。
		if e.presetAbstainForGone(agg) {
			changed = true
		}
		if e.maybeSettleVoting(agg) {
			changed = true
		}

	case domain.StateSummarizing:
		// 获胜者超时没写 → 机械兜底（§八 #10：必须产出结果）。
		//
		// ★ 窗口固定从"进入 summarizing"起算，默认 120s，不按获胜者的活动续期。
		//   为什么不需要续期：agent 是被长轮询**唤醒**的（状态一变就唤醒，不用等 wait 超时），
		//   它拿到 youAreWinner=true 的那一刻就几乎等于窗口起点。续期只会把最坏情况翻倍，
		//   而救不回任何真会写的 agent —— 写不出来的，由 saved:false + summary_too_late
		//   如实告诉它（见 Summary()），而不是让它以为自己写成了。
		ms := e.summaryTO.Milliseconds()
		if ms > 0 && e.now()-r.PhaseStartedAt >= ms {
			if r.SummaryText == "" {
				r.SummaryText = MechanicalSummary(agg)
				r.SummaryAuthor = "系统（机械兜底）"
				r.SummaryAt = e.now()
			}
			e.endRoom(agg, "summary_timeout")
			changed = true
		}
	}
	return changed
}

func (e *Engine) endRoom(agg *Agg, reason string) {
	r := agg.Room
	if r.State == domain.StateEnded {
		return
	}
	r.State = domain.StateEnded
	r.EndedAt = e.now()
	r.EndReason = reason
	if r.SummaryText == "" {
		// 任何路径都必须有概述
		r.SummaryText = MechanicalSummary(agg)
		if r.SummaryAuthor == "" {
			r.SummaryAuthor = "系统（机械兜底）"
		}
		r.SummaryAt = e.now()
	}
	res := buildResult(agg)
	res.Health = e.healthOf(agg)
	r.ResultJSON = MustMarshalResult(res)
	r.Plan = nil
}

// MustMarshalResult 把结果序列化成冻结快照。
func MustMarshalResult(res *Result) string {
	b, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	return string(b)
}

// =============================================================================
// 建会议
// =============================================================================

// CreateInput 是 POST /rooms 的请求体。
type CreateInput struct {
	Topic           string `json:"topic"`
	Rules           string `json:"rules"`
	Prompt          string `json:"prompt"` // 可选：自定义引导词
	TokenBudget     int    `json:"tokenBudget"`
	MaxMinutes      int    `json:"maxMinutes"`
	MaxRounds       int    `json:"maxRounds"`
	MaxAgents       int    `json:"maxAgents"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	BriefingMS      int    `json:"briefingMs"`
}

// CreateResult 是建会结果。
type CreateResult struct {
	Room     *domain.Room
	Password string
	AdminKey string
	Estimate domain.Estimate
}

// Create 建会议。
func (e *Engine) Create(ctx context.Context, tenantID string, in CreateInput) (*CreateResult, error) {
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		return nil, domain.NewError(400, "bad_request", "缺 topic：会议必须有议题")
	}

	tb := in.TokenBudget
	if tb <= 0 {
		tb = e.def.TokenBudget
	}
	if tb < 1000 {
		tb = 1000
	}
	ml := in.MaxMinutes
	if ml <= 0 {
		ml = e.def.MaxMinutes
	}
	if ml < 1 {
		ml = 1
	}
	// §八 #22：轮数超过硬上限一律夹到 10
	mr := in.MaxRounds
	if mr <= 0 {
		mr = e.def.MaxRounds
	}
	if mr > CapMaxRounds {
		mr = CapMaxRounds
	}
	if mr < 2 {
		mr = 2
	}
	ma := in.MaxAgents
	if ma <= 0 {
		ma = e.def.MaxAgents
	}
	if ma > CapMaxAgents {
		ma = CapMaxAgents
	}
	if ma < 1 {
		ma = 1
	}
	out := in.MaxOutputTokens
	if out <= 0 {
		out = e.def.MaxOutputTokens
	}

	briefMS := e.briefing.Milliseconds()
	if in.BriefingMS > 0 {
		briefMS = int64(in.BriefingMS)
	}
	if briefMS < 500 {
		briefMS = 500
	}

	roomID, err := randCode("r_", 4, true)
	if err != nil {
		return nil, err
	}
	roomNo, err := e.uniqueRoomNo(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	password := mustRand(4) + "-" + mustRand(4)
	adminKey := "adm_" + mustHex(18)

	r := &domain.Room{
		ID: roomID, TenantID: tenantID, RoomNo: roomNo,
		Topic: topic, Rules: strings.TrimSpace(in.Rules),
		State:            domain.StateBriefing,
		MaxRounds:        mr,
		MaxAgents:        ma,
		MaxMinutes:       ml,
		MaxOutputTokens:  out,
		TokenBudget:      tb,
		PasswordHash:     domain.HashToken(password),
		AdminKeyHash:     domain.HashToken(adminKey),
		CreatedAt:        e.now(),
		BriefingDeadline: e.now() + briefMS,
	}
	_ = in.Prompt // 自定义 prompt 摘要暂不落库；brief.prompt 按 §六 动态拼

	if err := e.store.WithTenant(ctx, tenantID, func(tx *store.Tx) error {
		if err := tx.CreateRoom(r); err != nil {
			return err
		}
		return tx.PutRoomDirectory(r.ID, r.RoomNo)
	}); err != nil {
		return nil, err
	}
	e.runtimeFor(tenantID, roomID)

	return &CreateResult{Room: r, Password: password, AdminKey: adminKey, Estimate: estimateFor(r)}, nil
}

// uniqueRoomNo 生成 9 位会议号，重试直到不与既有会议冲突。
//
// 用 crypto/rand 而不是自增：会议号是"报给别人的一串数字"，
// 自增等于把"我们有几场会"和"下一场是多少"直接告诉任何人。
func (e *Engine) uniqueRoomNo(ctx context.Context, tenantID string) (string, error) {
	for i := 0; i < 20; i++ {
		no, err := randomDigits(9)
		if err != nil {
			return "", err
		}
		// 用全局路由表做唯一性检查（room_no 上有唯一索引兜底）
		if _, _, err := e.store.ResolveRoomKey(ctx, no); err != nil {
			if _, ok := err.(*domain.Error); ok {
				return no, nil // 不存在 = 可用
			}
			return "", err
		}
	}
	return "", fmt.Errorf("生成会议号失败：连续冲突")
}

func randomDigits(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteString(v.String())
	}
	s := b.String()
	// 不允许以 0 开头（报号码时容易歧义）
	if strings.HasPrefix(s, "0") {
		s = "1" + s[1:]
	}
	return s, nil
}

// FormatRoomNo 把 9 位数字排成 "728 451 936"。
func FormatRoomNo(no string) string {
	if len(no) != 9 {
		return no
	}
	return no[0:3] + " " + no[3:6] + " " + no[6:9]
}

// estimateFor 是机械估算：预期发言数 = 人数 × 轮数。
func estimateFor(r *domain.Room) domain.Estimate {
	const lowPerMsg, highPerMsg = 60, 150
	planned := r.MaxAgents * r.MaxRounds
	fanout := r.MaxAgents - 1
	if fanout < 0 {
		fanout = 0
	}
	low := planned * (lowPerMsg + lowPerMsg*fanout)
	high := planned * (highPerMsg + highPerMsg*fanout)
	return domain.Estimate{
		Tokens:    (low + high) / 2,
		Low:       low,
		High:      high,
		WorstCase: high,
		Basis:     fmt.Sprintf("按 %d 条发言、每条 %d–%d est_tokens、每条被其余 %d 个 agent 各读一次估算", planned, lowPerMsg, highPerMsg, fanout),
		Fits:      high <= r.TokenBudget,
	}
}

// =============================================================================
// 小工具
// =============================================================================

func mustRand(n int) string {
	s, err := randCode("", n, false)
	if err != nil {
		return strings.Repeat("X", n)
	}
	return s
}

func mustHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func randCode(prefix string, n int, lower bool) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	s := string(out)
	if lower {
		s = strings.ToLower(s)
	}
	return prefix + s, nil
}

// fmtNum 加千分位。
func fmtNum(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}
