package host

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agchat/internal/budget"
	"agchat/internal/domain"
	"agchat/internal/invite"
	"agchat/internal/oplog"
	"agchat/internal/room"
	"agchat/internal/store"
)

// Host 是本地宿主：一个进程服务一个 home 下的**所有**房间（1 进程 / 1 库 / 1 ticker）。
type Host struct {
	cfg Config
	st  *store.Store
	eng *room.Engine

	ctlToken string

	mu    sync.Mutex
	rooms map[string]*roomCtx // ref → room
	byID  map[string]*roomCtx // roomId → room

	ctx       context.Context
	cancel    context.CancelFunc
	srv       *http.Server
	ln        net.Listener
	addr      string
	startedAt int64
	done      chan struct{}
}

// roomCtx 是一场会的宿主侧状态（密钥、日志、座位）。
type roomCtx struct {
	ref      string
	roomID   string
	password string
	adminKey string
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey

	log *opLog

	mu        sync.Mutex
	seats     map[string]*seat // agentID → seat
	join      map[string]*seat // joinKey → seat
	byPub     map[string]*seat // agentPub(b64) → seat
	inv       map[string]*inviteRec
	snap      snapInfo
	ended     bool
	dirty     bool  // 状态文件待落盘
	lastAlive int64 // 上一次"宿主确实活着并处理过东西"的时刻（resume 时算停机时长）
}

type seat struct {
	AgentID  string   `json:"agentId"`
	Alias    string   `json:"alias"`
	PubB64   string   `json:"agentPub"`
	Token    string   `json:"token,omitempty"`
	Caps     []string `json:"caps"`
	InviteID string   `json:"inviteId"`
	Nonce    string   `json:"nonce"`
	JoinKey  string   `json:"joinKey"`
	Ord      int      `json:"ord"`

	pub ed25519.PublicKey `json:"-"`
}

type inviteRec struct {
	ID        string `json:"id"`
	Uses      int    `json:"uses"`
	Exp       int64  `json:"exp"`
	Nonce     string `json:"nonce"`
	Card      string `json:"card,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// stateFile 是 rooms/<ref>/seats.json（0600）：座位凭据与已发卡列表。
//
// 为什么落盘：resume 之后座位必须还能用（AC4），而 agent token 明文只在
// join 响应里出现过一次 —— 宿主不留一份就无法"重复 joinKey 返回同一座位"（A-14）。
type stateFile struct {
	V         int          `json:"v"`
	Seats     []*seat      `json:"seats"`
	Invites   []*inviteRec `json:"invites"`
	LastAlive int64        `json:"lastAlive"`
}

func (c Config) statePath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "seats.json")
}

// snapInfo 是宿主的"轻量房间视图"：ticker 只在它上面做整数比较，
// 没有到期的房间一次 DB 都不碰（用户要求空闲时 CPU ≈ 0）。
type snapInfo struct {
	State             domain.State
	Round             int
	MaxRounds         int
	MaxAgents         int
	MaxMinutes        int
	CreatedAt         int64
	BriefingDeadline  int64
	StartedAt         int64
	PhaseStartedAt    int64
	EndedAt           int64
	TurnStartedAt     int64
	PlanPos           int
	QueueLen          int
	Seq               int
	Agents            int
	Votes             int
	BudgetUsed        int
	TokenBudget       int
	EndReason         string
	ConvergenceReason string
	WinnerProposalID  string
	VotesSeen         []string
	PlanSkipped       []string
}

func snapOf(agg *room.Agg) snapInfo {
	r := agg.Room
	s := snapInfo{
		State: r.State, Round: r.Round, MaxRounds: r.MaxRounds, MaxAgents: r.MaxAgents,
		MaxMinutes: r.MaxMinutes, CreatedAt: r.CreatedAt, BriefingDeadline: r.BriefingDeadline,
		StartedAt: r.StartedAt, PhaseStartedAt: r.PhaseStartedAt, EndedAt: r.EndedAt,
		Seq: r.Seq, Agents: len(agg.Agents), Votes: len(agg.Votes),
		BudgetUsed: r.BudgetUsed, TokenBudget: r.TokenBudget,
		EndReason: r.EndReason, ConvergenceReason: r.ConvergenceReason,
		WinnerProposalID: r.WinnerProposalID,
	}
	if r.Plan != nil {
		s.TurnStartedAt = r.Plan.TurnStartedAt
		s.PlanPos = r.Plan.Pos
		s.QueueLen = len(r.Plan.Queue)
		s.PlanSkipped = append([]string{}, r.Plan.Skipped...)
	}
	seen := map[string]bool{}
	for _, v := range agg.Votes {
		seen[v.AgentID] = true
	}
	s.VotesSeen = sortedKeys(seen)
	return s
}

// New 造一个宿主：开库、装引擎、加载 home 下已有的房间。
func New(cfg Config) (*Host, error) {
	cfg.ensureDefaults()
	if err := ensureHome(cfg); err != nil {
		return nil, err
	}
	ctl, err := ControlToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("控制面 token: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	eng := room.New(st, cfg.TurnTimeout, cfg.Briefing, cfg.SummaryTimeout)
	eng.SetVoteGrace(cfg.VoteGrace)
	// ★ S1 的唯一时序决定（arch R-2 / docs/09 §5）：关掉 presence-abstain。
	eng.SetPresenceAbstain(false)

	h := &Host{
		cfg: cfg, st: st, eng: eng, ctlToken: ctl,
		rooms: map[string]*roomCtx{}, byID: map[string]*roomCtx{},
		done: make(chan struct{}),
	}
	if err := h.loadRooms(); err != nil {
		_ = st.Close()
		return nil, err
	}
	return h, nil
}

// Config 返回宿主实际生效的配置（CLI 打印用）。
func (h *Host) Config() Config { return h.cfg }

// Addr 返回监听地址（Serve 之后才有值）。
func (h *Host) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// loadRooms 从 <Home>/rooms/*/host.key 恢复所有房间。
func (h *Host) loadRooms() error {
	entries, err := os.ReadDir(h.cfg.roomsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ctx := context.Background()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ref := e.Name()
		if err := h.openRoom(ctx, ref); err != nil {
			// 单个房间坏掉不能拖垮整个宿主：跳过并继续（其它房间照常开会）。
			fmt.Fprintf(os.Stderr, "跳过房间 %s: %v\n", ref, err)
		}
	}
	return nil
}

// openRoom 加载一场会（幂等）。
func (h *Host) openRoom(ctx context.Context, ref string) error {
	h.mu.Lock()
	_, ok := h.rooms[ref]
	h.mu.Unlock()
	if ok {
		return nil
	}
	var sec roomSecret
	if err := readJSON(h.cfg.roomSecretPath(ref), &sec); err != nil {
		return fmt.Errorf("读 host.key: %w", err)
	}
	priv, err := privFromB64(sec.RoomPriv)
	if err != nil {
		return err
	}
	pub, err := pubFromB64(sec.RoomPub)
	if err != nil {
		return err
	}
	lg, err := loadOpLog(h.cfg.opsPath(ref))
	if err != nil {
		return fmt.Errorf("读 ops.jsonl: %w", err)
	}
	rc := &roomCtx{
		ref: ref, roomID: sec.RoomID, password: sec.Password, adminKey: sec.AdminKey,
		priv: priv, pub: pub, log: lg,
		seats: map[string]*seat{}, join: map[string]*seat{}, byPub: map[string]*seat{},
		inv: map[string]*inviteRec{},
	}
	if err := h.loadState(rc); err != nil {
		_ = lg.close()
		return err
	}
	// G9：停机恢复时把绝对时间戳整体后移停机时长，再记一条 resume_clock_adjust。
	// 阈值 2s：正常重启（几乎瞬时）不需要动时间，避免平白的时钟漂移。
	if rc.lastAlive > 0 {
		if dt := nowMillis() - rc.lastAlive; dt > 2000 {
			if err := h.adjustClockForResume(ctx, rc, dt); err != nil {
				fmt.Fprintf(os.Stderr, "房间 %s 时钟修正失败（继续）: %v\n", ref, err)
			}
		}
	}
	snap, err := h.peek(ctx, rc)
	if err != nil {
		_ = lg.close()
		return fmt.Errorf("载入房间状态: %w", err)
	}
	rc.snap = snap
	rc.ended = snap.State == domain.StateEnded
	h.touch(rc)
	h.persist(rc)
	h.mu.Lock()
	h.rooms[ref] = rc
	h.byID[rc.roomID] = rc
	h.mu.Unlock()
	return nil
}

func (h *Host) loadState(rc *roomCtx) error {
	var sf stateFile
	err := readJSON(h.cfg.statePath(rc.ref), &sf)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, s := range sf.Seats {
		if s == nil {
			continue
		}
		pub, err := pubFromB64(s.PubB64)
		if err != nil {
			continue
		}
		s.pub = pub
		rc.seats[s.AgentID] = s
		rc.join[s.JoinKey] = s
		rc.byPub[s.PubB64] = s
	}
	for _, iv := range sf.Invites {
		if iv != nil {
			rc.inv[iv.ID] = iv
		}
	}
	rc.lastAlive = sf.LastAlive
	return nil
}

func (h *Host) saveState(rc *roomCtx) error {
	sf := stateFile{V: 1}
	for _, s := range rc.seats {
		sf.Seats = append(sf.Seats, s)
	}
	sort.Slice(sf.Seats, func(i, j int) bool { return sf.Seats[i].AgentID < sf.Seats[j].AgentID })
	for _, iv := range rc.inv {
		sf.Invites = append(sf.Invites, iv)
	}
	sort.Slice(sf.Invites, func(i, j int) bool { return sf.Invites[i].CreatedAt < sf.Invites[j].CreatedAt })
	if rc.lastAlive == 0 {
		rc.lastAlive = nowMillis()
	}
	sf.LastAlive = rc.lastAlive
	rc.dirty = false
	return writeJSON(h.cfg.statePath(rc.ref), sf, true)
}

// touch 刷新"宿主还活着"的时间戳（resume 时用它算停机时长，G9）。
func (h *Host) touch(rc *roomCtx) {
	rc.lastAlive = nowMillis()
	rc.dirty = true
}

// persist 把状态文件落盘（best-effort；失败不影响会议继续）。
func (h *Host) persist(rc *roomCtx) {
	if rc.dirty {
		_ = h.saveState(rc)
	}
}

// adjustClockForResume 把房间的绝对时间戳整体后移停机时长（G9）。
//
// 为什么必须做：引擎的所有时序判定都读绝对时间戳。宿主停机 1 小时再 resume，
// 如果不整体后移，第一次 tick 就会认为"投票窗口早已截止 / 回合超时太久"，
// 于是把一场还活着的会议直接结算掉 —— 断点恢复就变成了"恢复即结束"。
func (h *Host) adjustClockForResume(ctx context.Context, rc *roomCtx, downtime int64) error {
	if downtime <= 0 {
		return nil
	}
	err := h.st.WithTenant(ctx, TenantID, func(tx *store.Tx) error {
		r, err := tx.GetRoom(rc.roomID)
		if err != nil {
			return err
		}
		if r.State == domain.StateEnded {
			return nil
		}
		r.CreatedAt += downtime
		if r.BriefingDeadline > 0 {
			r.BriefingDeadline += downtime
		}
		if r.StartedAt > 0 {
			r.StartedAt += downtime
		}
		if r.PhaseStartedAt > 0 {
			r.PhaseStartedAt += downtime
		}
		if r.RoundIdleSince > 0 {
			r.RoundIdleSince += downtime
		}
		if r.Plan != nil {
			r.Plan.TurnStartedAt += downtime
		}
		return tx.SaveRoom(r)
	})
	if err != nil {
		return err
	}
	// 记一条 advance，重放时能知道"这里时间被人为平移过"（G9）。
	op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpAdvance, "host",
		map[string]any{
			"reason":        oplog.ReasonResumeClockAdjust,
			"downtimeMs":    downtime,
			"deadlineHit":   false,
			"allVoted":      false,
			"votesSeen":     []string{},
			"seatLastOpSeq": map[string]int{},
		})
	if err != nil {
		return err
	}
	return h.commitHostOp(rc, op)
}

// Serve 绑定端口、写 rendezvous、起 ticker，然后阻塞到 ctx 取消。
func (h *Host) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	h.ctx, h.cancel = ctx, cancel
	h.startedAt = nowMillis()

	addr := h.cfg.Addr
	if h.cfg.ExposeLAN && strings.HasPrefix(addr, "127.0.0.1") {
		addr = strings.Replace(addr, "127.0.0.1", "0.0.0.0", 1)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("绑定 %s 失败（可能已有宿主在跑，先用 agchat room status 看一眼）: %w", addr, err)
	}
	h.mu.Lock()
	h.ln = ln
	h.addr = ln.Addr().String()
	rooms := make([]*roomCtx, 0, len(h.rooms))
	for _, rc := range h.rooms {
		rooms = append(rooms, rc)
	}
	h.mu.Unlock()

	h.writeRendezvousFiles(rooms)

	h.srv = &http.Server{
		Handler:           h.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	room.SafeGo("host-ticker", func() { h.runTicker(ctx) })

	errCh := make(chan error, 1)
	room.SafeGo("host-http", func() { errCh <- h.srv.Serve(ln) })

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = h.srv.Shutdown(shutCtx)
	h.Close()
	return nil
}

// Close 关掉所有房间日志与数据库。
func (h *Host) Close() {
	h.mu.Lock()
	rooms := make([]*roomCtx, 0, len(h.rooms))
	for _, rc := range h.rooms {
		rooms = append(rooms, rc)
	}
	h.mu.Unlock()
	for _, rc := range rooms {
		rc.mu.Lock()
		if rc.dirty {
			_ = h.saveState(rc)
		}
		_ = rc.log.close()
		rc.mu.Unlock()
	}
	_ = h.st.Close()
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

func (h *Host) writeRendezvousFiles(rooms []*roomCtx) {
	_ = writeHomeRendezvous(h.cfg, h.addr, h.startedAt)
	for _, rc := range rooms {
		_ = writeRendezvous(h.cfg, rc.ref, h.addr, h.startedAt)
	}
}

func (h *Host) roomList() []*roomCtx {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*roomCtx, 0, len(h.rooms))
	for _, rc := range h.rooms {
		out = append(out, rc)
	}
	return out
}

func (h *Host) roomByRef(ref string) *roomCtx {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[ref]
}

func (h *Host) roomByID(id string) *roomCtx {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byID[id]
}

// =============================================================================
// 轻量 ticker（唯一允许触发时间跃迁的两条路径之一）
// =============================================================================

func (h *Host) runTicker(ctx context.Context) {
	t := time.NewTicker(h.cfg.TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := nowMillis()
			for _, rc := range h.roomList() {
				if !h.deadlineDue(rc, now) {
					continue // 没到期：一次 DB 都不碰（空闲 CPU ≈ 0）
				}
				func(rc *roomCtx) {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(os.Stderr, "tick panic room=%s: %v\n", rc.ref, r)
						}
					}()
					if err := h.tickPass(ctx, rc); err != nil {
						fmt.Fprintf(os.Stderr, "tick 房间 %s: %v\n", rc.ref, err)
					}
				}(rc)
			}
		}
	}
}

// deadlineDue 只读内存快照做整数比较。
func (h *Host) deadlineDue(rc *roomCtx, now int64) bool {
	rc.mu.Lock()
	s := rc.snap
	rc.mu.Unlock()
	if s.State == domain.StateEnded {
		return false
	}
	switch s.State {
	case domain.StateBriefing:
		return s.BriefingDeadline > 0 && now >= s.BriefingDeadline
	case domain.StateDebating:
		if s.MaxMinutes > 0 && s.CreatedAt > 0 && now >= s.CreatedAt+int64(s.MaxMinutes)*60000 {
			return true
		}
		if s.TurnStartedAt > 0 && now >= s.TurnStartedAt+h.cfg.TurnTimeout.Milliseconds() {
			return true
		}
		if s.TokenBudget > 0 && s.BudgetUsed*100/s.TokenBudget >= 95 {
			return true
		}
		return false
	case domain.StateVoting:
		return s.PhaseStartedAt > 0 && now >= s.PhaseStartedAt+room.VotingWindow.Milliseconds()
	case domain.StateSummarizing:
		ms := h.cfg.SummaryTimeout.Milliseconds()
		return ms > 0 && s.PhaseStartedAt > 0 && now >= s.PhaseStartedAt+ms
	}
	return false
}

// tickPass 是"先推进再操作"的那次推进：跑一次只做 tick 的 Do，然后
// 把中间发生的跃迁写成 advance/end op（G10）。
func (h *Host) tickPass(ctx context.Context, rc *roomCtx) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	before, err := h.peek(ctx, rc)
	if err != nil {
		return err
	}
	if err := h.eng.View(ctx, TenantID, rc.roomID, func(*room.Agg) error { return nil }); err != nil {
		return err
	}
	err = h.syncLocked(ctx, rc, before)
	if rc.dirty {
		h.persist(rc)
	}
	return err
}

// peek 只读载入聚合（不 tick、不落库）。
func (h *Host) peek(ctx context.Context, rc *roomCtx) (snapInfo, error) {
	var s snapInfo
	err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		s = snapOf(agg)
		return nil
	})
	return s, err
}

// syncLocked 在改动后刷新快照，并把跃迁写进日志（调用方须持有 rc.mu）。
func (h *Host) syncLocked(ctx context.Context, rc *roomCtx, before snapInfo) error {
	after, err := h.peek(ctx, rc)
	if err != nil {
		return err
	}
	rc.snap = after
	if after.State != before.State || after.Round != before.Round ||
		after.PlanPos != before.PlanPos || after.Seq != before.Seq || after.EndedAt != before.EndedAt {
		if err := h.logTransition(rc, before, after); err != nil {
			return err
		}
	}
	if after.State == domain.StateEnded && !rc.ended {
		rc.ended = true
	}
	h.touch(rc)
	return nil
}

// reasonFor 把一次跃迁映射到 docs/09 §10.2⑥ 的闭枚 reason code。
func reasonFor(before, after snapInfo) string {
	switch after.State {
	case domain.StateEnded:
		switch after.EndReason {
		case "summary_timeout":
			return oplog.ReasonSummaryTimeout
		case "no_winner":
			return oplog.ReasonNoWinner
		case "admin_close":
			return oplog.ReasonAdminClose
		case "time_limit":
			return oplog.ReasonTimeLimit
		case "no_participants", "solo_agent":
			return oplog.ReasonSummaryDone
		}
		return oplog.ReasonSummaryDone
	case domain.StateVoting:
		switch after.ConvergenceReason {
		case "budget":
			return oplog.ReasonBudget
		case "time_limit":
			return oplog.ReasonTimeLimit
		case "rounds_done":
			return oplog.ReasonRoundsDone
		case "empty_rounds":
			return oplog.ReasonRoundIdle
		case "admin_close":
			return oplog.ReasonAdminClose
		}
		return oplog.ReasonRoundsDone
	case domain.StateSummarizing:
		if before.State == domain.StateVoting {
			return oplog.ReasonVotesComplete
		}
		return oplog.ReasonSummaryDone
	}
	// debating 内部：轮次推进 / 跳人
	if before.State == domain.StateBriefing {
		// 开场不是 §10.2⑥ 里的跃迁（枚举里没有 briefing→debating）：
		// 它由 create 的 briefingDeadline 与座位数完全决定，重放能推出来。
		// 返回空串表示"不写 advance"。
		return ""
	}
	if after.Round != before.Round {
		if after.Seq == before.Seq {
			return oplog.ReasonRoundIdle
		}
		return oplog.ReasonRoundsDone
	}
	return oplog.ReasonTurnTimeout
}

// logTransition 写 advance / end op（host 用 roomKey 签）。
func (h *Host) logTransition(rc *roomCtx, before, after snapInfo) error {
	if after.State == domain.StateEnded && before.State != domain.StateEnded {
		return h.logEnd(rc, after)
	}
	reason := reasonFor(before, after)
	if reason == "" {
		return nil
	}
	deadlineHit := false
	switch after.State {
	case domain.StateVoting:
		deadlineHit = before.State == domain.StateDebating &&
			before.MaxMinutes > 0 && before.CreatedAt > 0 &&
			after.PhaseStartedAt > 0
	case domain.StateSummarizing:
		deadlineHit = before.State == domain.StateVoting &&
			before.PhaseStartedAt > 0 &&
			after.PhaseStartedAt-before.PhaseStartedAt >= room.VotingWindow.Milliseconds()
	case domain.StateDebating:
		deadlineHit = reason == oplog.ReasonTurnTimeout && before.TurnStartedAt > 0 &&
			after.TurnStartedAt > 0 && after.TurnStartedAt-before.TurnStartedAt >= h.cfg.TurnTimeout.Milliseconds()
	}
	p := map[string]any{
		"reason":        reason,
		"deadlineHit":   deadlineHit,
		"allVoted":      after.Agents > 0 && after.Votes >= after.Agents,
		"votesSeen":     nonNilStrings(after.VotesSeen),
		"seatLastOpSeq": h.seatLastOpSeq(rc),
	}
	op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpAdvance, "host", p)
	if err != nil {
		return err
	}
	return h.commitHostOp(rc, op)
}

// logEnd 写 end op（arch B-14 的终态快照）。
func (h *Host) logEnd(rc *roomCtx, s snapInfo) error {
	reason := oplog.ReasonSummaryDone
	switch s.EndReason {
	case "summary_timeout":
		reason = oplog.ReasonSummaryTimeout
	case "no_winner":
		reason = oplog.ReasonNoWinner
	case "admin_close":
		reason = oplog.ReasonAdminClose
	case "time_limit":
		reason = oplog.ReasonTimeLimit
	}
	snap := map[string]any{
		"v":                 1,
		"state":             string(s.State),
		"round":             s.Round,
		"emptyRounds":       0,
		"roundSkipped":      false,
		"endReason":         s.EndReason,
		"convergenceReason": s.ConvergenceReason,
		"winnerProposalId":  s.WinnerProposalID,
		"phaseStartedAt":    s.PhaseStartedAt,
		"startedAt":         s.StartedAt,
		"endedAt":           s.EndedAt,
		"seq":               s.Seq,
		"planSkipped":       nonNilStrings(s.PlanSkipped),
	}
	p := map[string]any{"reason": reason, "snapshot": snap}
	op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpEnd, "host", p)
	if err != nil {
		return err
	}
	return h.commitHostOp(rc, op)
}

// seatLastOpSeq 返回每个座位"最后一条已提交 op 的链内序号"（R-3 判定输入之一）。
func (h *Host) seatLastOpSeq(rc *roomCtx) map[string]int {
	out := map[string]int{}
	for _, op := range rc.log.committedOps() {
		if op.A == "host" || op.A == "" {
			continue
		}
		out[op.A] = op.I
	}
	return out
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// commitHostOp 提交一条 host 自己签的 op（create/invite/advance/end/reject）。
//
// ★ 必须**先写 op 行（intent）再写 commit 标记**：ops.jsonl 是唯一证据，
// 只写 commit 不写 op 行会让重启后的链序号回退（踩过：resume 后 i 重号）。
func (h *Host) commitHostOp(rc *roomCtx, op oplog.Op) error {
	if _, err := rc.log.intent(op); err != nil {
		return err
	}
	ack, err := oplog.SignAck(rc.priv, rc.ref, op.I, op.H, op.T)
	if err != nil {
		return err
	}
	return rc.log.commit(op, commitRec{OK: true, Ack: &ack, T: nowMillis()})
}

// =============================================================================
// 建会 / 发卡 / 关会 / 状态
// =============================================================================

// CreateReq 是 POST /v1/room 的请求体。
type CreateReq struct {
	Topic       string `json:"topic"`
	Rules       string `json:"rules,omitempty"`
	Seats       int    `json:"seats,omitempty"`
	Rounds      int    `json:"rounds,omitempty"`
	Minutes     int    `json:"minutes,omitempty"`
	TokenBudget int    `json:"tokenBudget,omitempty"`
	MaxOutput   int    `json:"maxOutput,omitempty"`
}

// CreateResp 是建会结果。
type CreateResp struct {
	Ref       string          `json:"ref"`
	RoomID    string          `json:"roomId"`
	Topic     string          `json:"topic"`
	Seats     int             `json:"seats"`
	MaxRounds int             `json:"maxRounds"`
	Minutes   int             `json:"minutes"`
	RoomNo    string          `json:"roomNo"`
	Addr      string          `json:"addr"`
	HostFile  string          `json:"hostFile"`
	DB        string          `json:"db"`
	Estimate  domain.Estimate `json:"estimate"`
}

// Create 建一场会：engine.Create + roomKey + genesis op + rendezvous。
func (h *Host) Create(ctx context.Context, req CreateReq) (*CreateResp, error) {
	pub, priv, err := newRoomKey()
	if err != nil {
		return nil, err
	}
	ref := refOf(pub)

	created, err := h.eng.Create(ctx, TenantID, room.CreateInput{
		Topic: req.Topic, Rules: req.Rules, MaxAgents: req.Seats,
		MaxRounds: req.Rounds, MaxMinutes: req.Minutes,
		TokenBudget: req.TokenBudget, MaxOutputTokens: req.MaxOutput,
	})
	if err != nil {
		return nil, err
	}
	rc := &roomCtx{
		ref: ref, roomID: created.Room.ID, password: created.Password, adminKey: created.AdminKey,
		priv: priv, pub: pub,
		seats: map[string]*seat{}, join: map[string]*seat{}, byPub: map[string]*seat{},
		inv: map[string]*inviteRec{}, dirty: true,
	}
	// 密钥落盘（0600）。密码同样只留在本机：卡里没有、日志里也没有（G7）。
	sec := roomSecret{
		V: 1, Ref: ref, RoomID: rc.roomID, Password: created.Password, AdminKey: created.AdminKey,
		RoomPriv: privB64(priv), RoomPub: pubB64(pub), CreatedAt: nowMillis(),
	}
	if err := writeJSON(h.cfg.roomSecretPath(ref), sec, true); err != nil {
		return nil, err
	}
	lg, err := loadOpLog(h.cfg.opsPath(ref))
	if err != nil {
		return nil, err
	}
	rc.log = lg
	if err := h.saveState(rc); err != nil {
		_ = lg.close()
		return nil, err
	}

	r := created.Room
	payload := map[string]any{
		"ref": ref, "key": pubB64(pub), "agentPub": pubB64(pub),
		"topic": r.Topic, "seats": r.MaxAgents, "maxRounds": r.MaxRounds,
		"maxAgents": r.MaxAgents, "maxMinutes": r.MaxMinutes, "maxOutput": r.MaxOutputTokens,
		"tokenBudget": r.TokenBudget, "roomNo": FormatRoomNo(r.RoomNo),
		"createdAt": r.CreatedAt, "briefingDeadline": r.BriefingDeadline,
		"ids":   []any{},
		"rv":    map[string]any{"res": 1, "vote": 1, "hash": 1},
		"hn":    h.cfg.DisplayName,
		"on":    h.cfg.OwnerName,
		"alias": h.cfg.OwnerName,
		"model": "host",
		"agent": "host",
	}
	op, err := oplog.Seal(priv, oplog.ZeroHash, 1, nowMillis(), oplog.OpCreate, "host", payload)
	if err != nil {
		_ = lg.close()
		return nil, err
	}
	if err := h.commitHostOp(rc, op); err != nil {
		_ = lg.close()
		return nil, err
	}
	snap, err := h.peek(ctx, rc)
	if err != nil {
		_ = lg.close()
		return nil, err
	}
	rc.snap = snap

	h.mu.Lock()
	h.rooms[ref] = rc
	h.byID[rc.roomID] = rc
	addr := h.addr
	h.mu.Unlock()

	if addr != "" {
		_ = writeRendezvous(h.cfg, ref, addr, h.startedAt)
	}
	return &CreateResp{
		Ref: ref, RoomID: rc.roomID, Topic: r.Topic, Seats: r.MaxAgents, MaxRounds: r.MaxRounds,
		Minutes: r.MaxMinutes, RoomNo: FormatRoomNo(r.RoomNo), Addr: addr,
		HostFile: h.cfg.roomJSONPath(ref), DB: h.cfg.DBPath, Estimate: created.Estimate,
	}, nil
}

// FormatRoomNo 与 room.FormatRoomNo 同口径（避免 CLI 为了打印再引一次）。
func FormatRoomNo(no string) string { return room.FormatRoomNo(no) }

// IssueReq 是 POST /v1/invite 的请求体。
type IssueReq struct {
	Ref  string `json:"ref"`
	Uses int    `json:"uses,omitempty"`
	Exp  int64  `json:"exp,omitempty"`
}

// IssueResp 是发卡结果。
type IssueResp struct {
	Ref     string   `json:"ref"`
	Card    string   `json:"card"`
	Human   string   `json:"human"`
	Machine string   `json:"machine"`
	IDs     []string `json:"ids"`
	Uses    int      `json:"uses"`
	Seats   int      `json:"seats"`
	Exp     int64    `json:"exp"`
}

// Issue 发一张卡（docs/09 §2）。
func (h *Host) Issue(ctx context.Context, req IssueReq) (*IssueResp, error) {
	rc := h.roomByRef(req.Ref)
	if rc == nil {
		return nil, errNotFound
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	snap, err := h.peek(ctx, rc)
	if err != nil {
		return nil, err
	}
	uses := req.Uses
	if uses <= 0 {
		uses = 1
	}
	if uses > snap.MaxAgents {
		uses = snap.MaxAgents
	}
	inviteID, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	inviteID = "iv_" + inviteID
	nonce, err := invite.NewNonce()
	if err != nil {
		return nil, err
	}
	eps := h.endpoints()
	var roomTopic string
	if err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		roomTopic = agg.Room.Topic
		return nil
	}); err != nil {
		return nil, err
	}
	payload := invite.Payload{
		V: invite.CardVersion, API: invite.APIVersion,
		Ref: rc.ref, Key: pubB64(rc.pub),
		IDs: []string{inviteID}, Uses: uses, Seats: snap.MaxAgents,
		Exp: req.Exp, IAT: nowMillis(),
		TH: invite.TopicHash(roomTopic),
		HN: h.cfg.DisplayName, ON: h.cfg.OwnerName,
		Caps: []string{invite.CapJoin, invite.CapRead, invite.CapPost, invite.CapPropose, invite.CapVote},
		RV:   invite.Rules{Res: invite.RulesVersion, Vote: invite.RulesVersion, Hash: invite.RulesVersion},
		EP:   eps, N: nonce,
	}
	human := h.humanLines(rc, &payload, roomTopic)
	card, err := invite.IssueLayered(payload, rc.priv, human)
	if err != nil {
		return nil, newOpErr(500, "card_issue_failed", "发卡失败: "+err.Error())
	}
	machine, err := invite.Issue(payload, rc.priv)
	if err != nil {
		return nil, newOpErr(500, "card_issue_failed", "发卡失败: "+err.Error())
	}
	// invite op：记下 id / uses / exp（verify 要用它核对卡的 ids）
	op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpInvite, "host",
		map[string]any{"id": inviteID, "uses": uses, "exp": req.Exp, "seats": snap.MaxAgents})
	if err != nil {
		return nil, err
	}
	if err := h.commitHostOp(rc, op); err != nil {
		return nil, err
	}
	rc.inv[inviteID] = &inviteRec{ID: inviteID, Uses: uses, Exp: req.Exp, Nonce: nonce, Card: card, CreatedAt: nowMillis()}
	h.touch(rc)
	rc.dirty = true
	if err := h.saveState(rc); err != nil {
		return nil, err
	}
	rc.snap = snap
	return &IssueResp{
		Ref: rc.ref, Card: card, Machine: machine,
		Human: strings.SplitN(card, invite.BeginMarker, 2)[0],
		IDs:   []string{inviteID}, Uses: uses, Seats: snap.MaxAgents, Exp: req.Exp,
	}, nil
}

// Card 返回最近一次发出的卡（--card-file 复看用）。
func (h *Host) Card(ref string) (string, error) {
	rc := h.roomByRef(ref)
	if rc == nil {
		return "", errNotFound
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var last *inviteRec
	for _, iv := range rc.inv {
		if last == nil || iv.CreatedAt >= last.CreatedAt {
			last = iv
		}
	}
	if last == nil || last.Card == "" {
		return "", newOpErr(404, "no_card", "这场会还没有发过卡")
	}
	return last.Card, nil
}

// endpoints 返回卡里的直连端点（默认 loopback；--expose-lan 追加 lan + ins:1）。
func (h *Host) endpoints() []invite.Endpoint {
	addr := h.Addr()
	if addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	eps := []invite.Endpoint{{K: invite.EndpointLoopback, U: "http://127.0.0.1:" + port}}
	if h.cfg.ExposeLAN {
		for _, ip := range lanIPs() {
			eps = append(eps, invite.Endpoint{K: invite.EndpointLAN, U: "http://" + net.JoinHostPort(ip, port), INS: 1})
		}
	}
	return eps
}

func lanIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// humanLines 组织人读块：四要素顺序固定（谁开的 / 议什么 / 几人 / 什么时候前可进）。
func (h *Host) humanLines(rc *roomCtx, p *invite.Payload, topic string) []string {
	when := "不限时前可进"
	if p.Exp > 0 {
		when = time.UnixMilli(p.Exp).Local().Format("01-02 15:04") + " 前可进"
	}
	netKind := "本机"
	if h.cfg.ExposeLAN {
		netKind = "本机/内网"
	}
	lines := []string{
		fmt.Sprintf("%s 的 agent 邀你参加「%s」· %d 人 · %s · %s", p.ON, topic, p.Seats, when, netKind),
		"卡号（念这 8 位核对）：" + invite.ShortRef(p.Ref),
		"怎么连：把下面机读块整段发给我，我的 agent 自己会连（本机/内网直连）",
	}
	if h.cfg.ExposeLAN {
		lines = append(lines, "注意：宿主已开放内网（明文 HTTP），仅限可信网络。")
	}
	lines = append(lines, "本方案没有 PKI：卡的签名只证明签发者持有 roomKey 私钥，不证明签发者是某个人。")
	return lines
}

// StatusResp 是房间的人读进展（G13：需要控制面 token）。
type StatusResp struct {
	Ref       string         `json:"ref"`
	RoomID    string         `json:"roomId"`
	Topic     string         `json:"topic"`
	RoomNo    string         `json:"roomNo"`
	State     domain.State   `json:"state"`
	Round     int            `json:"round"`
	MaxRounds int            `json:"maxRounds"`
	Seats     int            `json:"seats"`
	Joined    int            `json:"joined"`
	Messages  int            `json:"messages"`
	Proposals int            `json:"proposals"`
	Votes     int            `json:"votes"`
	Budget    map[string]int `json:"budget"`
	Members   []MemberView   `json:"members"`
	EndReason string         `json:"endReason,omitempty"`
	Winner    string         `json:"winnerProposalId,omitempty"`
	Head      HeadView       `json:"head"`
	CreatedAt int64          `json:"createdAt"`
	StartedAt int64          `json:"startedAt"`
	EndedAt   int64          `json:"endedAt"`
}

// MemberView 是 status 里的成员一行（alias 只是展示，不是授权依据）。
type MemberView struct {
	AgentID string `json:"agentId"`
	Alias   string `json:"alias"`
	Model   string `json:"model"`
	Ord     int    `json:"ord"`
	Voted   bool   `json:"voted"`
}

// HeadView 是链头（客户端签名前要先拿到它）。
type HeadView struct {
	I int    `json:"i"`
	H string `json:"h"`
}

// Status 返回房间进展。**不 tick**（arch R-7 / G10）。
func (h *Host) Status(ctx context.Context, ref string) (*StatusResp, error) {
	rc := h.roomByRef(ref)
	if rc == nil {
		return nil, errNotFound
	}
	var out *StatusResp
	err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		r := agg.Room
		members := make([]MemberView, 0, len(agg.Agents))
		voted := map[string]bool{}
		for _, v := range agg.Votes {
			voted[v.AgentID] = true
		}
		for _, a := range agg.Agents {
			members = append(members, MemberView{AgentID: a.ID, Alias: a.Alias, Model: a.Model, Ord: a.Ord, Voted: voted[a.ID]})
		}
		led := budget.View(r.BudgetUsed, r.TokenBudget)
		out = &StatusResp{
			Ref: rc.ref, RoomID: rc.roomID, Topic: r.Topic, RoomNo: FormatRoomNo(r.RoomNo),
			State: r.State, Round: r.Round, MaxRounds: r.MaxRounds, Seats: r.MaxAgents,
			Joined: len(agg.Agents), Messages: len(agg.Messages), Proposals: len(agg.Proposals),
			Votes:   len(agg.Votes),
			Budget:  map[string]int{"tokenBudget": led.Total, "used": led.Used, "remaining": led.Left, "pct": led.Pct},
			Members: members, EndReason: r.EndReason, Winner: r.WinnerProposalID,
			Head:      HeadView{I: len(rc.log.committedOps()), H: rc.log.head()},
			CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRooms 列出 home 下所有房间的极简信息（room status 无 --ref 时用）。
func (h *Host) ListRooms(ctx context.Context) []*StatusResp {
	var out []*StatusResp
	for _, rc := range h.roomList() {
		if st, err := h.Status(ctx, rc.ref); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// CloseRoom 由宿主用自己的 adminKey 关会（A-11：不引入对外 adminKey）。
func (h *Host) CloseRoom(ctx context.Context, ref string) (*StatusResp, error) {
	rc := h.roomByRef(ref)
	if rc == nil {
		return nil, errNotFound
	}
	rc.mu.Lock()
	before, err := h.peek(ctx, rc)
	if err == nil {
		_, err = h.eng.Close(ctx, TenantID, rc.roomID, rc.adminKey)
	}
	if err == nil {
		err = h.syncLocked(ctx, rc, before)
	}
	rc.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return h.Status(ctx, ref)
}

// =============================================================================
// 加入
// =============================================================================

// JoinReq 是 POST /v1/join 的请求体。
type JoinReq struct {
	Card     string `json:"card"`
	Ref      string `json:"ref,omitempty"`
	AgentPub string `json:"agentPub"`
	Nonce    string `json:"nonce"`
	Alias    string `json:"alias,omitempty"`
	Model    string `json:"model,omitempty"`
	DryRun   bool   `json:"dryRun,omitempty"`
}

// Consent 是同意门要给人看的三行（pm P1 / AC8）。
type Consent struct {
	Ref         string         `json:"ref"`
	HostName    string         `json:"hostName"`
	Owner       string         `json:"owner"`
	Topic       string         `json:"topic"`
	Seats       int            `json:"seats"`
	Joined      int            `json:"joined"`
	Rounds      int            `json:"rounds"`
	Minutes     int            `json:"minutes"`
	JoinBy      int64          `json:"joinBy"`
	TokenBudget int            `json:"tokenBudget"`
	Estimate    map[string]int `json:"estimate"`
	Caps        []string       `json:"caps"`
}

// JoinResp 是加入结果（dryRun 时只有 Consent）。
type JoinResp struct {
	Joined      bool         `json:"joined"`
	Status      string       `json:"status,omitempty"`
	Consent     *Consent     `json:"consent,omitempty"`
	AgentID     string       `json:"agentId,omitempty"`
	Alias       string       `json:"alias,omitempty"`
	Token       string       `json:"token,omitempty"`
	IsModerator bool         `json:"isModerator,omitempty"`
	Duplicate   bool         `json:"duplicate,omitempty"`
	Reclaimed   bool         `json:"reclaimed,omitempty"`
	Caps        []string     `json:"caps,omitempty"`
	Head        HeadView     `json:"head"`
	State       domain.State `json:"state,omitempty"`
	KeyFile     string       `json:"keyFile,omitempty"`
	Receipts    string       `json:"receiptsFile,omitempty"`
}

// Join 让持卡 agent 进场。卡是唯一凭证；密码由宿主内部补上（G7）。
func (h *Host) Join(ctx context.Context, in JoinReq) (*JoinResp, error) {
	// 两条入口：带卡（首次进场 / 换卡再进）与只带 ref + 私钥（认回自己的座位）。
	//
	// 第二条为什么必须存在：agent 的运行环境经常是一次性的沙箱，进程一换 token 就没了。
	// 如果只能靠卡认回，卡又在主人那条聊天记录里，agent 就得回头找人要卡 —— 那正是
	// 这个产品要消灭的来回。私钥还在手上就够了。
	var (
		rc       *roomCtx
		inviteID string
		caps     []string
		card     *invite.Card
	)
	if strings.TrimSpace(in.Card) != "" {
		var err error
		card, err = invite.ParseAt(in.Card, nowMillis())
		if err != nil {
			return nil, &opError{Status: 400, Code: "card_unusable", Msg: invite.HumanError(err), Detail: map[string]any{"human": invite.HumanError(err)}}
		}
		rc = h.roomByRef(card.P.Ref)
		if rc == nil {
			return nil, &opError{Status: 404, Code: "card_wrong_room", Msg: invite.HumanWrongRoom}
		}
		if err := card.Verify(rc.pub); err != nil {
			return nil, &opError{Status: 401, Code: "card_bad_signature", Msg: invite.HumanError(err), Detail: map[string]any{"human": invite.HumanError(err)}}
		}
		if err := invite.VerifyRoom(card, rc.ref); err != nil {
			return nil, &opError{Status: 409, Code: "card_wrong_room", Msg: invite.HumanWrongRoom, Detail: map[string]any{"human": invite.HumanError(err)}}
		}
		if card.P.RV != (invite.Rules{Res: invite.RulesVersion, Vote: invite.RulesVersion, Hash: invite.RulesVersion}) {
			return nil, &opError{Status: 409, Code: "rules_mismatch", Msg: invite.HumanRulesMismatch}
		}
		if ok, reason := admit(card.P.Caps, invite.CapJoin); !ok {
			return nil, &opError{Status: 403, Code: "capability_denied", Msg: reason}
		}
		inviteID = card.P.IDs[0]
		caps = card.P.Caps
	} else if strings.TrimSpace(in.Ref) != "" {
		rc = h.roomByRef(strings.TrimSpace(in.Ref))
		if rc == nil {
			return nil, &opError{Status: 404, Code: "card_wrong_room", Msg: invite.HumanWrongRoom}
		}
	} else {
		return nil, newOpErr(400, "card_or_ref_required", "要么给卡（--card），要么给 ref（--ref）认回座位")
	}

	pub, err := pubFromB64(in.AgentPub)
	if err != nil {
		return nil, newOpErr(400, "bad_agent_pub", "agentPub 必须是 32 字节 base64url 的 Ed25519 公钥")
	}
	if strings.TrimSpace(in.Nonce) == "" {
		return nil, newOpErr(400, "nonce_required", "必须带 nonce（joinKey 幂等键的一半）")
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	// 无卡认回：座位与能力都取自那一座自己的记录。
	if card == nil {
		prev, ok := rc.byPub[pubB64(pub)]
		if !ok {
			return nil, newOpErr(404, "not_seated", "这把私钥在这场会里没有座位；首次进场请用 --card")
		}
		inviteID = prev.InviteID
		caps = prev.Caps
	}
	jk := joinKey(pub, inviteID, in.Nonce)

	if card != nil {
		consent, err := h.consentLocked(ctx, rc, card)
		if err != nil {
			return nil, err
		}
		if in.DryRun {
			return &JoinResp{Joined: false, Status: "consent_required", Consent: consent,
				Head: HeadView{I: len(rc.log.committedOps()), H: rc.log.head()}}, nil
		}
	}
	_ = caps

	// 幂等：同一 joinKey 不重复扣 uses、返回同一座位（A-14）。
	if s, ok := rc.join[jk]; ok {
		return &JoinResp{
			Joined: true, Status: "already_joined", AgentID: s.AgentID, Alias: s.Alias, Token: s.Token,
			Duplicate: true, Caps: s.Caps,
			Head:  HeadView{I: len(rc.log.committedOps()), H: rc.log.head()},
			State: rc.snap.State,
		}, nil
	}

	// 认回（A-18/A-19）：同一个 agentPub 回来过，就还它原来的座位。
	//
	// 为什么必须靠 agentPub 而不是 alias：随机密码被宿主内部收走了，alias 又是
	// 展示字段（可能重名、可能是别人起的）。只有私钥能证明"我就是原来那个座位"。
	// 这是宿主路径与 agchatd 的差异点，不能照抄 ops.go 的 alias 认回。
	var prevSeat *seat
	if prev, ok := rc.byPub[pubB64(pub)]; ok {
		prevSeat = prev
		model := strings.TrimSpace(in.Model)
		if model == "" {
			model = prev.Alias
		}
		before, perr := h.peek(ctx, rc)
		if perr != nil {
			return nil, perr
		}
		res, rerr := h.eng.Join(ctx, TenantID, rc.roomID, room.JoinInput{
			Password: rc.password, Alias: prev.Alias, Model: model, Reclaim: true, ForceReclaim: true,
		}, h.Addr())
		if rerr == nil {
			if err := h.syncLocked(ctx, rc, before); err != nil {
				return nil, err
			}
			prev.Token = res.AgentToken
			op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpJoin, "host",
				map[string]any{
					"inviteId": prev.InviteID, "agentPub": prev.PubB64, "agentId": prev.AgentID,
					"alias": prev.Alias, "ord": prev.Ord, "joinKey": jk, "reclaim": true,
				})
			if err != nil {
				return nil, err
			}
			if err := h.commitHostOp(rc, op); err != nil {
				return nil, err
			}
			rc.join[jk] = prev
			h.touch(rc)
			rc.dirty = true
			if err := h.saveState(rc); err != nil {
				return nil, err
			}
			return &JoinResp{
				Joined: true, Status: "reclaimed", AgentID: prev.AgentID, Alias: prev.Alias,
				Token: prev.Token, Reclaimed: true, Caps: prev.Caps,
				Head:  HeadView{I: len(rc.log.committedOps()), H: rc.log.head()},
				State: rc.snap.State,
			}, nil
		}
		// 30 秒防双开窗口内旧实例可能还活着：只要还有空位，就给它新开一个座位，
		// 不要因为"刚崩过一次"就被挡在门外；坐满了才把排队信息报回去。
		if !isMemberActive(rerr) || len(rc.seats) >= rc.snap.MaxAgents {
			return nil, toOpErr(rerr)
		}
	}
	// uses 校验：这张卡还能不能再进一个
	iv := rc.inv[inviteID]
	if iv == nil {
		return nil, newOpErr(404, "invite_unknown", "这张卡的 inviteId 不在本场会的签发记录里")
	}
	used := 0
	for _, s := range rc.seats {
		// 认回失败后的兜底新座位：如果这一座本来就用同一张卡，别再算一次它的占用
		// （否则"一次性卡 + 刚崩过"会直接撞 invite_exhausted）。
		if s.InviteID == inviteID && s != prevSeat {
			used++
		}
	}
	if used >= iv.Uses {
		return nil, newOpErr(409, "invite_exhausted", fmt.Sprintf("这张卡的 %d 个座位已用完（每张卡都是可数的）", iv.Uses))
	}
	if rc.snap.State != domain.StateBriefing && rc.snap.State != domain.StateDebating {
		return nil, newOpErr(410, "room_started", "会议已开始，不再接受新参会者")
	}

	// alias 只做展示：宿主强制唯一，冲突加后缀（A-18）。授权只认 token/agentPub。
	taken := map[string]bool{}
	for _, s := range rc.seats {
		taken[s.Alias] = true
	}
	alias := uniqueAlias(in.Alias, taken)
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = "unknown"
	}

	// R-5：先推进再操作。
	before, err := h.peek(ctx, rc)
	if err != nil {
		return nil, err
	}
	res, err := h.eng.Join(ctx, TenantID, rc.roomID, room.JoinInput{
		Password: rc.password, Alias: alias, Model: model,
	}, h.Addr())
	if err != nil {
		return nil, toOpErr(err)
	}
	if err := h.syncLocked(ctx, rc, before); err != nil {
		return nil, err
	}

	seat := &seat{
		AgentID: res.AgentID, Alias: alias, PubB64: pubB64(pub), Token: res.AgentToken,
		Caps: append([]string{}, card.P.Caps...), InviteID: inviteID, Nonce: in.Nonce,
		JoinKey: jk, Ord: res.SpeakingOrder, pub: pub,
	}
	// join op（host 签：座位/ord 是宿主分配的，agent 在此之前的签名无法覆盖它们）
	op, err := oplog.Seal(rc.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpJoin, "host",
		map[string]any{
			"inviteId": inviteID, "agentPub": pubB64(pub), "agentId": res.AgentID,
			"alias": alias, "ord": res.SpeakingOrder, "joinKey": jk,
		})
	if err != nil {
		return nil, err
	}
	if err := h.commitHostOp(rc, op); err != nil {
		return nil, err
	}
	rc.seats[res.AgentID] = seat
	rc.join[jk] = seat
	rc.byPub[pubB64(pub)] = seat
	h.touch(rc)
	rc.dirty = true
	if err := h.saveState(rc); err != nil {
		return nil, err
	}
	return &JoinResp{
		Joined: true, AgentID: res.AgentID, Alias: alias, Token: res.AgentToken,
		IsModerator: res.IsModerator, Caps: seat.Caps,
		Head:    HeadView{I: len(rc.log.committedOps()), H: rc.log.head()},
		State:   rc.snap.State,
		KeyFile: h.cfg.SeatPath(rc.ref), Receipts: h.cfg.ReceiptsPath(rc.ref),
	}, nil
}

func (h *Host) consentLocked(ctx context.Context, rc *roomCtx, card *invite.Card) (*Consent, error) {
	var c *Consent
	err := h.eng.PeekReadOnly(ctx, TenantID, rc.roomID, func(agg *room.Agg) error {
		r := agg.Room
		c = &Consent{
			Ref: rc.ref, HostName: h.cfg.DisplayName, Owner: card.P.ON, Topic: r.Topic,
			Seats: r.MaxAgents, Joined: len(agg.Agents), Rounds: r.MaxRounds, Minutes: r.MaxMinutes,
			JoinBy: card.P.Exp, TokenBudget: r.TokenBudget,
			Estimate: estimateOf(r), Caps: card.P.Caps,
		}
		return nil
	})
	return c, err
}

// estimateOf 与 room 的机械估算同口径（人数 × 轮数 × 每条发言的读放大）。
func estimateOf(r *domain.Room) map[string]int {
	const lowPerMsg, highPerMsg = 60, 150
	planned := r.MaxAgents * r.MaxRounds
	fanout := r.MaxAgents - 1
	if fanout < 0 {
		fanout = 0
	}
	low := planned * (lowPerMsg + lowPerMsg*fanout)
	high := planned * (highPerMsg + highPerMsg*fanout)
	return map[string]int{"tokens": (low + high) / 2, "low": low, "high": high}
}

// toOpErr 把引擎的 *domain.Error 透成宿主错误（状态码/码/人话都不改）。
func toOpErr(err error) error {
	var de *domain.Error
	if errors.As(err, &de) {
		e := &opError{Status: de.Status, Code: de.Code, Msg: de.Message}
		if len(de.Extra) > 0 {
			e.Detail = de.Extra
		}
		return e
	}
	if errors.Is(err, domain.ErrNotFound) {
		return errNotFound
	}
	return err
}

// =============================================================================
// 参与者 op
// =============================================================================

// AgentOpReq 是 post/propose/vote/summary 的请求体。
//
// ★ opKey 放在**外层**而不是签名载荷里：docs/09 §4.1 冻结了 p 的字段集，
// 而 opKey 只是宿主的幂等索引（不参与链哈希、不影响结论）。
type AgentOpReq struct {
	Op    oplog.Op `json:"op"`
	OpKey string   `json:"opKey,omitempty"`
}

// applyAgentOp 是四个改状态端点共用的唯一入口：
// 验 token → 验签 → 能力（admit）→ 幂等 → 先推进 → 日志先行 → 引擎 → commit/ack。
func (h *Host) applyAgentOp(ctx context.Context, kind, token string, req AgentOpReq) (any, *oplog.Ack, error) {
	rc, s, err := h.authByToken(token)
	if err != nil {
		return nil, nil, err
	}
	return h.applyOnSeat(ctx, rc, s, kind, req)
}

func (h *Host) applyOnSeat(ctx context.Context, rc *roomCtx, s *seat, kind string, req AgentOpReq) (any, *oplog.Ack, error) {
	op := req.Op
	if op.O != kind {
		return nil, nil, newOpErr(400, "bad_op_kind", fmt.Sprintf("这条 op 的类型是 %q，不是 %q", op.O, kind))
	}
	if strings.TrimSpace(op.A) != s.AgentID {
		return nil, nil, newOpErr(403, "author_mismatch", "op.a 必须是你自己的 agentId（alias 不是授权依据）")
	}
	if ok, reason := admit(s.Caps, capForKind(kind)); !ok {
		return nil, nil, newOpErr(403, "capability_denied", reason)
	}
	// 幂等：同 (agent,kind,opKey) 只生效一次（G8）。
	if req.OpKey != "" {
		if cr, ok := rc.log.lookupIdem(s.AgentID, kind, req.OpKey); ok {
			var res any
			if len(cr.Res) > 0 {
				_ = json.Unmarshal(cr.Res, &res)
			}
			return res, nil, nil // duplicate：返回上次的结果，不重复生效
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	// R-5：先推进再操作。**必须在链前置条件之前**：这一步可能写一条 advance op，
	// 从而移动链头；否则客户端签名时的 i/ph 会与推进后的链头撞号。
	before, err := h.peek(ctx, rc)
	if err != nil {
		return nil, nil, err
	}
	if err := h.eng.View(ctx, TenantID, rc.roomID, func(*room.Agg) error { return nil }); err != nil {
		return nil, nil, err
	}
	if err := h.syncLocked(ctx, rc, before); err != nil {
		return nil, nil, err
	}
	// 推进之后的状态才是这次 op 的起点。
	before = rc.snap

	// 链前置条件：i 与 ph 必须接在当前链头上（客户端先取 head 再签名）。
	head := rc.log.head()
	if op.PH == "" {
		op.PH = head
	}
	next := rc.log.nextIndex()
	if op.I != next || op.PH != head {
		return nil, nil, newOpErr(409, "head_conflict", "链头变了：请重新 GET /v1/head 再签名").
			with("head", HeadView{I: next - 1, H: head})
	}
	h2, err := oplog.HashOp(op)
	if err != nil {
		return nil, nil, newOpErr(400, "bad_op", "op 不完整: "+err.Error())
	}
	if !strings.EqualFold(h2, op.H) {
		return nil, nil, newOpErr(400, "bad_op_hash", "op.h 与重算不一致")
	}
	if err := oplog.VerifySig(op.H, op.S, s.pub); err != nil {
		return nil, nil, newOpErr(401, "bad_signature", "op 签名验不过（必须用 seats/<ref>.key 里的私钥签）")
	}

	// 日志先行：先落 op 行，再让引擎执行。
	off, err := rc.log.intent(op)
	if err != nil {
		return nil, nil, err
	}
	res, resJSON, bizErr := h.runEngine(ctx, rc, s, kind, &op, req.OpKey)

	if bizErr != nil {
		// 被拒：截掉 intent，改写一条 host 签的 reject op（§4.1）。
		_ = rc.log.rollback(off)
		rej, rerr := oplog.Seal(rc.priv, head, next, nowMillis(), oplog.OpReject, "host", map[string]any{
			"attemptedOp": json.RawMessage(mustJSON(op)),
			"code":        errCode(bizErr),
			"message":     bizErr.Error(),
		})
		if rerr != nil {
			return nil, nil, rerr
		}
		ack, _ := oplog.SignAck(rc.priv, rc.ref, rej.I, rej.H, rej.T)
		if cerr := rc.log.commit(rej, commitRec{OK: false, Code: errCode(bizErr), Agent: s.AgentID,
			Kind: kind, OpKey: req.OpKey, Ack: &ack, T: nowMillis()}); cerr != nil {
			return nil, nil, cerr
		}
		return nil, nil, toOpErr(bizErr)
	}

	// 先提交 agent 的 op（链上它必须在 advance/end 之前），再补 op 自身引起的跃迁。
	ack, err := oplog.SignAck(rc.priv, rc.ref, op.I, op.H, op.T)
	if err != nil {
		return nil, nil, err
	}
	cr := commitRec{OK: true, Agent: s.AgentID, Kind: kind, OpKey: req.OpKey, Ack: &ack, Res: resJSON, T: nowMillis()}
	if seq, ok := seqOf(res); ok {
		cr.Seq = seq
	}
	if err := rc.log.commit(op, cr); err != nil {
		return nil, nil, err
	}
	// op 自身引起的状态跃迁（投票结算/概述结束等）也要留 advance/end。
	if err := h.syncLocked(ctx, rc, before); err != nil {
		return nil, nil, err
	}
	if rc.dirty {
		h.persist(rc)
	}
	return res, &ack, nil
}

func errCode(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	var oe *opError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return "error"
}

func seqOf(res any) (int, bool) {
	switch v := res.(type) {
	case *room.PostResult:
		return v.Seq, true
	case *room.ProposeResult:
		return v.Seq, true
	}
	return 0, false
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// runEngine 只做"把 op 载荷交给引擎"这件事 —— 任何业务判断都在 room 包里。
func (h *Host) runEngine(ctx context.Context, rc *roomCtx, s *seat, kind string, op *oplog.Op, opKey string) (any, json.RawMessage, error) {
	switch kind {
	case oplog.OpPost:
		var p struct {
			Text    string `json:"text"`
			ReplyTo *int   `json:"replyTo,omitempty"`
			Support string `json:"support,omitempty"`
		}
		if err := json.Unmarshal(op.P, &p); err != nil {
			return nil, nil, newOpErr(400, "bad_op", "post 载荷解析失败: "+err.Error())
		}
		res, err := h.eng.Post(ctx, TenantID, rc.roomID, tokenHashOf(s), room.PostInput{
			Text: p.Text, ReplyTo: p.ReplyTo, SupportProposalID: p.Support, OpKey: opKeyOrEmpty(opKey),
		})
		if err != nil {
			return nil, nil, err
		}
		return res, mustJSON(res), nil
	case oplog.OpPropose:
		var p struct {
			Title      string `json:"title"`
			Body       string `json:"body"`
			Supersedes string `json:"supersedes,omitempty"`
		}
		if err := json.Unmarshal(op.P, &p); err != nil {
			return nil, nil, newOpErr(400, "bad_op", "propose 载荷解析失败: "+err.Error())
		}
		res, err := h.eng.Propose(ctx, TenantID, rc.roomID, tokenHashOf(s), room.ProposeInput{
			Title: p.Title, Body: p.Body, OpKey: opKeyOrEmpty(opKey), Revises: p.Supersedes,
		})
		if err != nil {
			return nil, nil, err
		}
		return res, mustJSON(res), nil
	case oplog.OpVote:
		var p struct {
			ProposalID  *string `json:"proposalId,omitempty"`
			FinalStance string  `json:"finalStance,omitempty"`
			Reason      string  `json:"reason,omitempty"`
		}
		if err := json.Unmarshal(op.P, &p); err != nil {
			return nil, nil, newOpErr(400, "bad_op", "vote 载荷解析失败: "+err.Error())
		}
		res, err := h.eng.Vote(ctx, TenantID, rc.roomID, tokenHashOf(s), room.VoteInput{
			ProposalID: p.ProposalID, FinalStance: p.FinalStance, Reason: p.Reason, OpKey: opKeyOrEmpty(opKey),
		})
		if err != nil {
			return nil, nil, err
		}
		return res, mustJSON(res), nil
	case oplog.OpSummary:
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(op.P, &p); err != nil {
			return nil, nil, newOpErr(400, "bad_op", "summary 载荷解析失败: "+err.Error())
		}
		res, err := h.eng.Summary(ctx, TenantID, rc.roomID, tokenHashOf(s), room.SummaryInput{
			Text: p.Text, OpKey: opKeyOrEmpty(opKey),
		})
		if err != nil {
			return nil, nil, err
		}
		return res, mustJSON(res), nil
	}
	return nil, nil, newOpErr(400, "bad_op", "不支持的 op 类型: "+kind)
}

// tokenHashOf 返回座位 token 的哈希（引擎只认哈希）。
func tokenHashOf(s *seat) string { return domain.HashToken(s.Token) }

// isMemberActive 判断引擎的认回是否被"30 秒防双开窗口"挡下（ops.go 的 member_active）。
//
// 挡住不等于失败：旧实例很可能真的还活着，这时候新开一个座位比把人关在门外好；
// 只有坐满了才需要把这件事报回给调用方。
func isMemberActive(err error) bool {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code == "member_active"
	}
	return false
}

func opKeyOrEmpty(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		// 引擎对 post 要求非空 opKey（网络重试靠它去重）；
		// 但宿主已经用外层 opKey 做过幂等，这里给一个稳定兜底键。
		return "host_" + randomMust(8)
	}
	return k
}

func randomMust(n int) string {
	s, err := randomHex(n)
	if err != nil {
		return "0000"
	}
	return s
}

// authByToken 是 agent token 的唯一鉴权入口（alias 永不参与）。
func (h *Host) authByToken(token string) (*roomCtx, *seat, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, newOpErr(401, "token_required", "缺少 agent token（Authorization: Bearer <token>）")
	}
	hash := domain.HashToken(token)
	ref, ok, err := h.st.LookupTokenTenant(context.Background(), hash)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, newOpErr(401, "bad_token", "token 无效或已作废")
	}
	rc := h.roomByID(ref.RoomID)
	if rc == nil {
		return nil, nil, errNotFound
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	orig := rc.seats[ref.AgentID]
	if orig == nil {
		return nil, nil, newOpErr(401, "seat_unknown", "这个座位不在本场会的宿主记录里")
	}
	// 返回一份副本：调用方随后会再次加锁，指针共享会踩到并发边界。
	cp := *orig
	cp.Caps = append([]string{}, orig.Caps...)
	return rc, &cp, nil
}
