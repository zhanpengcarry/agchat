package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agchat/internal/budget"
	"agchat/internal/domain"
	"agchat/internal/store"
)

func budgetView(r *domain.Room) budget.Ledger { return budget.View(r.BudgetUsed, r.TokenBudget) }

// =============================================================================
// 对外视图
// =============================================================================

// MessageView 是发言的对外形状。
type MessageView struct {
	Seq      int      `json:"seq"`
	Round    int      `json:"round"`
	AgentID  string   `json:"agentId"`
	Alias    string   `json:"alias"`
	Model    string   `json:"model"`
	Text     string   `json:"text"`
	ReplyTo  *int     `json:"replyTo"`
	Stance   *string  `json:"stance"`
	Mentions []string `json:"mentions"`
	TS       string   `json:"ts"`
	// SupportProposalID 只有收尾轮的表态会有值：这条发言声明支持哪个方案。
	// 它落库过（见 store.msgCols），读得到才谈得上"结论可核对"。
	SupportProposalID string `json:"supportProposalId,omitempty"`
}

func viewMessage(m *domain.Message) MessageView {
	var st *string
	if m.Stance != "" {
		v := m.Stance
		st = &v
	}
	men := m.Mentions
	if men == nil {
		men = []string{}
	}
	return MessageView{
		Seq: m.Seq, Round: m.Round, AgentID: m.AgentID, Alias: m.Alias, Model: m.Model,
		Text: m.Text, ReplyTo: m.ReplyTo, Stance: st, Mentions: men, TS: m.TS,
		// 收尾轮的表态「我支持哪个方案」必须能被读到（人复盘要看，agent 也要能核对自己的表态）。
		SupportProposalID: m.SupportProposalID,
	}
}

// RosterItem 是开场后下发的真实参会名单。
type RosterItem struct {
	Alias       string `json:"alias"`
	Model       string `json:"model"`
	IsModerator bool   `json:"isModerator"`
}

// Snapshot 是心跳返回体。
type Snapshot struct {
	Cursor      int64         `json:"cursor"`
	State       domain.State  `json:"state"`
	Round       int           `json:"round"`
	RoundsTotal int           `json:"roundsTotal"`
	YourTurn    bool          `json:"yourTurn"`
	IsModerator bool          `json:"isModerator"`
	Budget      budget.Ledger `json:"budget"`
	Unread      int           `json:"unread"`
	MentionsYou int           `json:"mentionsYou"`

	// Proposals 是方案列表（带票数）——不是计数。
	// 只有计数的话，agent 必须去调公开的 /result 才能知道方案 ID，语义上不该这样用。
	Proposals []ProposalView `json:"proposals"`
	// MyVote 是我投了哪个方案；没投为 null。
	MyVote *string `json:"myVote"`
	// ReadCursor 是我的**读消息水位**（message seq），GET /messages?since= 用它。
	//
	// ★ 房间里有**两套游标**，混用会拉不到东西：
	//   - inbox 的 cursor / since：事件序号（任何状态变化都 +1），只用于长轮询
	//   - messages 的 since / nextSince：消息 seq，只用于读正文
	//   inbox.cursor 通常远大于消息 seq，拿它去 /messages 一定返回空数组。
	//   两位干净 agent 都在这上面栽过一次，还各自白付了一次读取费用。
	ReadCursor int `json:"readCursor"`
	// YouAreWinner 只在 summarizing 阶段可能为 true：告诉获胜者"该你写概述了"。
	//
	// ★ 这个字段曾经只写在 SKILL.md 里、服务端根本没实现：
	//   照着文档判断 inbox.youAreWinner 的 agent 永远读不到 true，只能靠 notes
	//   里一句话猜；猜不到就干等满 60 秒，由机械摘要顶掉——人工写的概述永远进不了结果。
	//   现在由 snapshotOf 用 summaryAuthor 计算，与 POST /summary 的授权判定同一个函数。
	YouAreWinner bool `json:"youAreWinner"`
	// YourTurnDeadlineInSec 是我的回合还剩多少秒；不是我的回合为 null。
	// 真实 LLM 组织一段长回答要 10–30 秒，不给期限它只能瞎猜什么时候会被跳过。
	YourTurnDeadlineInSec *int `json:"yourTurnDeadlineInSec"`

	// Roster 在 briefing 阶段为 null；一开场就带上真实名单，
	// 这样每个 agent 看到的参会人数一致（prompt 里只有上限，名单以这里为准）。
	Roster []RosterItem `json:"roster"`

	Notes    []string `json:"notes"`
	Since    *int64   `json:"since"`
	Woke     bool     `json:"woke"`
	TimedOut bool     `json:"timedOut"`
}

// Limits 是 brief 里的硬上限。
type Limits struct {
	TokenBudget          int `json:"tokenBudget"`
	Used                 int `json:"used"`
	MaxMinutes           int `json:"maxMinutes"`
	MaxOutputTokens      int `json:"maxOutputTokens"`
	MaxProposalsPerAgent int `json:"maxProposalsPerAgent"`
}

// Brief 是加入后 agent 拿到的任务说明（§三）。
type Brief struct {
	RoomNo          string `json:"roomNo"`
	Topic           string `json:"topic"`
	Prompt          string `json:"prompt"`
	Rules           string `json:"rules"`
	Rounds          int    `json:"rounds"`
	Members         int    `json:"members"`
	Limits          Limits `json:"limits"`
	YouAreModerator bool   `json:"youAreModerator"`
	SkillURL        string `json:"skillUrl"`
}

// JoinInput 是 POST /rooms/{key}/join 的请求体。
type JoinInput struct {
	Password string `json:"password"`
	Alias    string `json:"alias"`
	Model    string `json:"model"`
	// Reclaim=true 表示"我是这个 alias 的老成员，环境重启丢了 token，请把座位还给我"。
	// 必须同时提供正确的房间密码 —— 只凭 alias 认回等于谁猜到名字就能接管。
	Reclaim bool `json:"reclaim"`
	// ForceReclaim=true 时跳过"30 秒内还有活动就不许顶掉"的防双开判断。
	//
	// 调用方只有在**比 alias 更强的凭证**下才该置位（本地宿主是座位公钥）：
	// 拿着私钥的人就是这一座的主人，旧进程即使还活着也只是个持废 token 的僵尸。
	// 保留 alias 路径的 30 秒窗口不动，agchatd 的行为因此完全不变。
	ForceReclaim bool `json:"forceReclaim"`
}

// JoinResult 是加入结果。
type JoinResult struct {
	AgentToken    string
	AgentID       string
	IsModerator   bool
	SpeakingOrder int
	Reclaimed     bool
	Brief         Brief
}

// ActiveWindowMs：最近这么久内有请求，就认为这个 agent 还活着（认回时拒绝双开）。
const ActiveWindowMs int64 = 30_000

// PostInput 是发言请求体。
type PostInput struct {
	Text              string `json:"text"`
	ReplyTo           *int   `json:"replyTo"`
	SupportProposalID string `json:"supportProposalId"`
	OpKey             string `json:"opKey"`
}

// PostResult 是发言结果。
type PostResult struct {
	Seq       int            `json:"seq"`
	Budget    budget.Ledger  `json:"budget"`
	State     domain.State   `json:"state"`
	Billed    map[string]int `json:"billed,omitempty"`
	YourTurn  *bool          `json:"yourTurn,omitempty"`
	Duplicate bool           `json:"duplicate,omitempty"`
	// SupportProposalID 回显"这条发言声明的支持对象"，让 agent 不必为了核实
	// "服务端有没有记住我的表态"再读一次消息（读是要计费的）。
	SupportProposalID string `json:"supportProposalId,omitempty"`
}

// ReadResult 是读消息结果。
type ReadResult struct {
	Messages  []MessageView  `json:"messages"`
	Budget    budget.Ledger  `json:"budget"`
	Billed    map[string]int `json:"billed"`
	More      bool           `json:"more"`
	NextSince int            `json:"nextSince"`
}

// ProposeInput 是提交方案的请求体。
type ProposeInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	OpKey string `json:"opKey"`
	// Revises 非空 = 这是对**自己某个方案**的修订：新版本将取代该方案（它的票也会迁过来）。
	// 「方案不可改」保持不变——旧版本原样留在库里，只是标记 superseded_by。
	Revises string `json:"revises"`
}

// ProposeResult 是方案提交结果。
type ProposeResult struct {
	ProposalID string `json:"proposalId"`
	Seq        int    `json:"seq"`
	Count      int    `json:"count"`
	// Supersedes = 这个新版本取代了谁（修订时才有）。
	Supersedes string `json:"supersedes,omitempty"`
	// Superseded = 被本版本取代的那个 ID（与 Supersedes 同值，语义从"旧版本视角"给，
	// 方便调用方一句话说清"p_1 已被 p_5 取代"）。
	Superseded string `json:"superseded,omitempty"`
}

// VoteInput 是投票请求体（正常投票 + §九 降级投票两种形态）。
type VoteInput struct {
	ProposalID  *string `json:"proposalId"`
	FinalStance string  `json:"finalStance"`
	Reason      string  `json:"reason"`
	OpKey       string  `json:"opKey"`
}

// VoteResult 是投票结果。
type VoteResult struct {
	Votes map[string]int `json:"votes"`
	Cast  int            `json:"cast"`
	State domain.State   `json:"state"`
	// RedirectedTo 非空 = 你投的方案已被更新的修订版取代，这一票已自动改记到该版本。
	RedirectedTo string `json:"redirectedTo,omitempty"`
}

// SummaryInput 是概述提交体。
type SummaryInput struct {
	Text  string `json:"text"`
	OpKey string `json:"opKey"`
}

// SummaryResult 是概述提交结果。
//
// OK    = 请求被正确处理（不代表这次写入生效）
// Saved = **你这一次的 text 是否就是结果里那一份**（幂等重放为 true，超时为 false）
// Reason= already_saved / summary_too_late / "" （刚写入成功）
type SummaryResult struct {
	OK     bool         `json:"ok"`
	Saved  bool         `json:"saved"`
	State  domain.State `json:"state"`
	Author string       `json:"author"`
	Reason string       `json:"reason,omitempty"`
	Note   string       `json:"note,omitempty"`
}

// =============================================================================
// 加入
// =============================================================================

// Join 让 agent 凭密码进入会议。第一个进入者成为主席。
func (e *Engine) Join(ctx context.Context, tenantID, roomID string, in JoinInput, host string) (*JoinResult, error) {
	var out *JoinResult

	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		e.tick(agg)
		r := agg.Room

		// ① 密码先行：两条路径（新建 / 认回）都要它
		if in.Password == "" || domain.HashToken(in.Password) != r.PasswordHash {
			return false, domain.NewError(401, "wrong_password", "会议密码不对")
		}

		// ② 认回：环境重启后 agent 丢了 token，但座位还占着 —— 必须能回来。
		//    条件：同 password（已验）+ 同 alias + 该 agent 存在。
		if in.Reclaim {
			alias := strings.TrimSpace(in.Alias)
			if alias != "" {
				if existing, err := tx.FindAgentByAlias(roomID, alias); err == nil {
					// 防双开：它还活着就不要顶掉（除非调用方有更强的凭证）
					if !in.ForceReclaim && existing.LastSeenAt > 0 && e.now()-existing.LastSeenAt < ActiveWindowMs {
						return false, domain.NewError(409, "member_active",
							"该成员最近 30 秒内仍有活动，可能还活着；请稍后重试或先关闭旧实例")
					}
					newToken := "agc_" + mustHex(18)
					newHash := domain.HashToken(newToken)
					if err := tx.DeleteTokenDirectoryByAgent(roomID, existing.ID); err != nil {
						return false, err
					}
					if err := tx.RotateAgentToken(roomID, existing.ID, newHash, e.now()); err != nil {
						return false, err
					}
					if err := tx.PutTokenDirectory(newHash, roomID, existing.ID); err != nil {
						return false, err
					}
					// 内存里的聚合也要同步 —— 注意必须改 **agg 里的那个对象**。
					// FindAgentByAlias 返回的是另一份副本，改它没用：随后的 saveAgg
					// 会用聚合里的旧哈希把数据库覆盖回去（这样新 token 会立刻失效、旧 token 复活）。
					target := agg.AgentByID(existing.ID)
					if target == nil {
						return false, domain.ErrNotFound
					}
					target.TokenHash = newHash
					target.LastSeenAt = e.now()

					out = &JoinResult{
						AgentToken:    newToken,
						AgentID:       target.ID,
						IsModerator:   target.IsModerator(r),
						SpeakingOrder: target.Ord + 1,
						Reclaimed:     true,
						Brief:         e.briefFor(agg, target, host),
					}
					return true, nil
				}
				// alias 不存在 → 当作新成员，走下面的新建路径
			}
		}

		// ③ 新建座位
		if len(agg.Agents) >= r.MaxAgents {
			return false, domain.NewError(409, "room_full", fmt.Sprintf("会议已满（上限 %d 人）", r.MaxAgents))
		}
		if r.State != domain.StateBriefing {
			return false, domain.NewError(410, "room_ended", "会议已开始，不再接受新参会者")
		}

		agentID := fmt.Sprintf("m_%d", len(agg.Agents)+1)
		token := "agc_" + mustHex(18)
		alias := strings.TrimSpace(in.Alias)
		if alias == "" {
			alias = agentID
		}
		model := strings.TrimSpace(in.Model)
		if model == "" {
			model = "unknown"
		}
		ord := len(agg.Agents)

		a := &domain.Agent{
			ID: agentID, TenantID: tenantID, RoomID: roomID,
			Alias: alias, Model: model,
			TokenHash:   domain.HashToken(token),
			JoinedAt:    e.now(),
			LastSeenAt:  e.now(), // 刚入场就算活跃：防止别人立刻把它"认回"顶掉
			Ord:         ord,
			QuotaTokens: r.TokenBudget / r.MaxAgents,
		}
		if err := tx.InsertAgent(a); err != nil {
			return false, err
		}
		if err := tx.PutTokenDirectory(a.TokenHash, roomID, agentID); err != nil {
			return false, err
		}
		agg.Agents = append(agg.Agents, a)

		// ★ 主席 = 第一个成功 join 的人。房间锁 + 事务保证并发下恰好一个。
		if r.ModeratorID == "" {
			r.ModeratorID = agentID
		}

		e.tick(agg) // 名额满了就开场

		out = &JoinResult{
			AgentToken:    token,
			AgentID:       agentID,
			IsModerator:   a.IsModerator(r),
			SpeakingOrder: ord + 1,
			Brief:         e.briefFor(agg, a, host),
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// briefFor 组装 join 响应里的 brief（新建与认回共用，保证两条路径口径一致）。
func (e *Engine) briefFor(agg *Agg, a *domain.Agent, host string) Brief {
	r := agg.Room
	led := budgetView(r)
	return Brief{
		RoomNo: FormatRoomNo(r.RoomNo),
		Topic:  r.Topic,
		Prompt: BuildPrompt(r, a),
		Rules:  r.Rules,
		Rounds: r.MaxRounds,
		// ★ Members 是**名额上限**（与 prompt 里「本场会议最多 N 位参会者」同一口径），
		//   不是"此刻已进房的人数"。
		//   曾经这里写 len(agg.Agents)：第一个进房的人看到 members:1，以为这场会只有自己，
		//   而 prompt 同一份响应里写着"最多 6 位"。两位干净 agent 各自独立报了这条歧义。
		//   实际到场名单在开场后由 inbox.roster / notes 给出。
		Members: r.MaxAgents,
		Limits: Limits{
			TokenBudget: r.TokenBudget, Used: led.Used,
			MaxMinutes: r.MaxMinutes, MaxOutputTokens: r.MaxOutputTokens,
			MaxProposalsPerAgent: MaxProposalsPerAgent,
		},
		YouAreModerator: a.IsModerator(r),
		SkillURL:        "http://" + host + "/skill.md",
	}
}

// =============================================================================
// 心跳 / 读
// =============================================================================

func (e *Engine) Inbox(ctx context.Context, tenantID, roomID, tokenHash string, since *int64, wait time.Duration) (*Snapshot, error) {
	rt := e.runtimeFor(tenantID, roomID)

	rt.mu.Lock()
	snap, event, err := e.snapshotLocked(ctx, tenantID, roomID, rt, tokenHash, since)
	if err != nil {
		rt.mu.Unlock()
		return nil, err
	}
	if since == nil || wait <= 0 || event > *since || snap.State == domain.StateEnded {
		rt.mu.Unlock()
		return snap, nil
	}
	ch := make(chan struct{}, 1)
	rt.waiters[ch] = struct{}{}
	rt.mu.Unlock()

	woke, timedOut := false, false
	select {
	case <-ch:
		woke = true
	case <-time.After(wait):
		timedOut = true
	case <-ctx.Done():
		rt.mu.Lock()
		delete(rt.waiters, ch)
		rt.mu.Unlock()
		return nil, ctx.Err()
	}

	rt.mu.Lock()
	snap2, _, err := e.snapshotLocked(ctx, tenantID, roomID, rt, tokenHash, since)
	rt.mu.Unlock()
	if err != nil {
		return nil, err
	}
	snap2.Woke = woke
	snap2.TimedOut = timedOut
	return snap2, nil
}

func (e *Engine) snapshotLocked(ctx context.Context, tenantID, roomID string, rt *runtime,
	tokenHash string, since *int64) (*Snapshot, int64, error) {

	var snap *Snapshot
	err := e.runLocked(ctx, tenantID, roomID, rt, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		// 心跳不改业务状态，saveAgg 不会执行，所以这里必须单独落库，
		// 否则"活着"这件事存不进去，掉线判定会把活人也算掉线。
		if err := tx.TouchAgent(roomID, a.ID, a.LastSeenAt); err != nil {
			return changed, err
		}
		snap = e.snapshotOf(agg, a, since)
		return changed, nil
	})
	if err != nil {
		return nil, 0, err
	}
	snap.Cursor = rt.event
	return snap, rt.event, nil
}

func (e *Engine) snapshotOf(agg *Agg, a *domain.Agent, since *int64) *Snapshot {
	snap := &Snapshot{
		State: agg.Room.State, Round: agg.Room.Round, RoundsTotal: agg.Room.MaxRounds,
		IsModerator: a.IsModerator(agg.Room),
		Budget:      budgetView(agg.Room),
		Proposals:   e.proposalViews(agg),
		Roster:      buildRoster(agg),
		Notes:       e.notesFor(agg, a), Since: since,
	}

	cur := currentTurnAgent(agg)
	snap.YourTurn = cur != nil && cur.ID == a.ID
	if snap.YourTurn && agg.Room.Plan != nil {
		left := e.turnTO.Milliseconds() - (e.now() - agg.Room.Plan.TurnStartedAt)
		if left < 0 {
			left = 0
		}
		sec := int((left + 999) / 1000)
		snap.YourTurnDeadlineInSec = &sec
	}

	if v := agg.VoteByAgent(a.ID); v != nil && v.ProposalID != "" {
		pid := v.ProposalID
		snap.MyVote = &pid
	}

	// summarizing 阶段点名写概述的人：与 Summary() 里的授权判定同一个函数，
	// 保证"inbox 说该你写"和"POST /summary 允许你写"绝不会互相矛盾。
	if agg.Room.State == domain.StateSummarizing {
		if author := e.summaryAuthor(agg); author != nil && author.ID == a.ID {
			snap.YouAreWinner = true
		}
	}

	unread := unreadOf(agg, a)
	snap.Unread = len(unread)
	snap.ReadCursor = a.ReadCursor
	for _, m := range unread {
		for _, id := range m.Mentions {
			if id == a.ID {
				snap.MentionsYou++
				break
			}
		}
	}
	return snap
}

// proposalViews 把方案连同票数一起给出去（inbox 用）。
// proposalViews 把方案连同票数一起给出去（inbox 用）。
//
// ★ 这里**刻意不带 body**：inbox 是长轮询，每 20 秒或每个事件都要重发一次，
//
//	而方案正文动辄几百字。三位验证 agent 独立报了同一件事：一次会议里 inbox 反复
//	回显全部方案正文，约 1.1k token/次，占它们宿主上下文成本的 5% 量级 —— 这笔钱
//	不在房间预算里（inbox 不计费），但在 agent 主人自己的账上。
//	要正文的去 GET /result（公开、一次拿全）；inbox 只负责"有哪些方案、各多少票"。
func (e *Engine) proposalViews(agg *Agg) []ProposalView {
	if len(agg.Proposals) == 0 {
		return []ProposalView{}
	}
	tally := TallyVotes(agg)
	// inbox 只列**当前采纳中**的方案：已被修订取代的版本不能再投，列出来只会让人投错。
	adopted := agg.AdoptedProposals()
	out := make([]ProposalView, 0, len(adopted))
	for _, p := range adopted {
		out = append(out, ProposalView{ProposalID: p.ID, Title: p.Title, By: p.Alias, Votes: tally[p.ID],
			Supersedes: p.Supersedes})
	}
	return out
}

// buildRoster 在开场后返回真实名单；briefing 阶段返回 nil（人数还没定）。
func buildRoster(agg *Agg) []RosterItem {
	if agg.Room.State == domain.StateBriefing || len(agg.Agents) == 0 {
		return nil
	}
	out := make([]RosterItem, 0, len(agg.Agents))
	for _, id := range turnOrder(agg) {
		a := agg.AgentByID(id)
		if a == nil {
			continue
		}
		out = append(out, RosterItem{Alias: a.Alias, Model: a.Model, IsModerator: a.IsModerator(agg.Room)})
	}
	return out
}

func (e *Engine) notesFor(agg *Agg, a *domain.Agent) []string {
	r := agg.Room
	notes := []string{}
	pct := budgetView(r).Pct

	if r.State == domain.StateBriefing {
		left := (r.BriefingDeadline - e.now()) / 1000
		if left < 0 {
			left = 0
		}
		notes = append(notes, fmt.Sprintf("等待参会者入场，%d 秒后自动开始（名额满会提前开始）", left))
	} else if len(agg.Agents) > 0 {
		// 开场后告诉每个人真实的参会规模与主席，避免 agent 拿 prompt 里的"上限"当实际人数
		modAlias := "无"
		if m := agg.Moderator(); m != nil {
			modAlias = m.Alias
		}
		notes = append(notes, fmt.Sprintf("实际参会 %d 人，主席：%s", len(agg.Agents), modAlias))
	}
	if r.RoundSkipped {
		notes = append(notes, fmt.Sprintf("第 %d 轮无人发言，已跳过（连续空轮 %d/%d，到 %d 次直接进投票）",
			r.Round, r.EmptyRounds, EmptyRoundsToVoting, EmptyRoundsToVoting))
	}
	// 投票阶段点名掉线者：它们已被预置弃权，活着的人不必干等
	if r.State == domain.StateVoting {
		limit := GoneAfterTurns * e.turnTO.Milliseconds()
		gone := 0
		for _, ag := range agg.Agents {
			if agg.VoteByAgent(ag.ID) != nil && ag.LastSeenAt > 0 && e.now()-ag.LastSeenAt > limit {
				gone++
				notes = append(notes, fmt.Sprintf("%s 已计弃权（若它回来投票会自动更正）", ag.Alias))
			}
		}
		if gone > 0 && len(agg.Votes) >= len(agg.Agents) && e.voteGrace > 0 {
			notes = append(notes, fmt.Sprintf("等待掉线成员补投（最多 %s）", e.voteGrace))
		}
	}
	switch r.State {
	case domain.StateDebating:
		if pct >= budget.WarnPct {
			notes = append(notes, fmt.Sprintf("预算已用 %d%%", pct))
		}
		if r.Round == 1 {
			notes = append(notes, "第 1 轮：给出你的立场和理由")
		} else if r.Round < r.MaxRounds {
			notes = append(notes, "交锋轮：必须 replyTo 别人的具体观点；可提交方案")
		} else {
			notes = append(notes, "收尾轮：说明你支持哪个方案及理由")
		}
	case domain.StateVoting:
		if len(agg.Proposals) == 0 {
			notes = append(notes, "没有方案，按立场投票：提交 finalStance (agree/disagree/abstain)")
		} else {
			notes = append(notes, fmt.Sprintf("投票中：从 %d 个方案里选一个（可改票，null=弃权）", len(agg.Proposals)))
		}
	case domain.StateSummarizing:
		if w := TallyWinner(agg); w != nil && w.AgentID == a.ID {
			notes = append(notes, "你的方案获胜了，请提交一段简要概述")
		} else {
			notes = append(notes, "等待获胜者撰写概述")
		}
	case domain.StateEnded:
		notes = append(notes, "会议已结束，请读结果")
	}

	cur := currentTurnAgent(agg)
	if r.State == domain.StateDebating {
		if cur != nil && cur.ID == a.ID {
			notes = append(notes, "轮到你了，可以发言")
		} else if cur != nil {
			notes = append(notes, fmt.Sprintf("等待 %s 发言", cur.Alias))
		}
	}
	if n := len(unreadOf(agg, a)); n > 0 {
		// 直接把 since 写出来：让对方自己去猜"该传 cursor 还是 seq"是设计缺陷，
		// 猜错就是一次白读（读是计费的）。
		notes = append(notes, fmt.Sprintf("有 %d 条未读，先 GET /messages?since=%d（读会扣预算；这个 since 是消息 seq，不是 inbox 的 cursor）",
			n, a.ReadCursor))
	}
	return notes
}

func unreadOf(agg *Agg, a *domain.Agent) []*domain.Message {
	var out []*domain.Message
	for _, m := range agg.Messages {
		if m.Seq > a.ReadCursor && m.AgentID != a.ID {
			out = append(out, m)
		}
	}
	return out
}

// Read 读正文（计费）。
// Read 读正文（计费）。
//
// excludeSelf=true 时不返回自己发过的消息（省 agent 宿主的上下文），但游标照旧推进。
// 默认 false：`/messages` 是"会议转写稿"，默认必须回得全 —— 隔离套件正是靠
// "发出去再读回来核对作者字段"来证明身份不可伪造的（用例 7），把默认行为改成
// "只回别人"会直接把这个黑盒判据打掉。省 token 用显式开关，别偷改契约。
func (e *Engine) Read(ctx context.Context, tenantID, roomID, tokenHash string, since int, excludeSelf bool) (*ReadResult, error) {
	var out *ReadResult
	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		r := agg.Room
		if float64(r.BudgetUsed) >= float64(r.TokenBudget)*readBillingStopAt {
			return changed, domain.NewError(402, "budget_exhausted", "预算已耗尽，无法继续读取").
				With(map[string]any{"budget": budgetView(r)})
		}
		var all []*domain.Message
		for _, m := range agg.Messages {
			if m.Seq > since {
				all = append(all, m)
			}
		}
		list := all
		if len(list) > MaxReadMessages {
			list = list[:MaxReadMessages]
		}
		billed := 0
		views := make([]MessageView, 0, len(list))
		for _, m := range list {
			// ★ 自己发过的消息**不计费**：文本本来就在它自己的上下文里，再收一次钱是纯浪费。
			//   （验证 agent 实测：notes 给的 since=<readCursor>=0 会把本人 seq1 再读一遍。）
			if m.AgentID == a.ID {
				if excludeSelf {
					continue
				}
				views = append(views, viewMessage(m))
				continue
			}
			billed += budget.EstTokens(m.Text)
			views = append(views, viewMessage(m))
		}
		charge(r, a, "read", billed)
		if len(list) > 0 {
			if last := list[len(list)-1].Seq; last > a.ReadCursor {
				a.ReadCursor = last
			}
		}
		next := since
		if len(list) > 0 {
			next = list[len(list)-1].Seq
		}
		out = &ReadResult{Messages: views, Budget: budgetView(r),
			Billed: map[string]int{"tokens": billed, "messages": len(views)},
			More:   len(all) > len(list), NextSince: next}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func charge(r *domain.Room, a *domain.Agent, kind string, tokens int) {
	if tokens <= 0 {
		return
	}
	if kind == "read" {
		a.TokensRead += tokens
		r.BudgetRead += tokens
	} else {
		a.TokensWritten += tokens
		r.BudgetWritten += tokens
	}
	a.TokensTotal += tokens
	r.BudgetUsed += tokens
}

// =============================================================================
// 发言
// =============================================================================

// Post 发言（第 1 轮立场 / 交锋轮 / 收尾轮）。
func (e *Engine) Post(ctx context.Context, tenantID, roomID, tokenHash string, in PostInput) (*PostResult, error) {
	var out *PostResult

	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		r := agg.Room

		opKey := strings.TrimSpace(in.OpKey)
		if opKey == "" {
			return changed, domain.NewError(400, "opKey_required", "必须带 opKey：网络重试靠它去重")
		}
		if seq, found, err := tx.FindOpKey(roomID, a.ID, opKey); err != nil {
			return changed, err
		} else if found {
			out = &PostResult{Seq: seq, Budget: budgetView(r), State: r.State, Duplicate: true}
			return changed, nil
		}

		if r.State == domain.StateEnded {
			return changed, domain.NewError(410, "room_ended", "会议已结束")
		}
		if r.State != domain.StateDebating {
			return changed, domain.NewError(403, "wrong_round",
				fmt.Sprintf("当前是 %s 阶段，不能发言", r.State))
		}

		cur := currentTurnAgent(agg)
		if cur == nil || cur.ID != a.ID {
			msg := "现在没有轮到任何人"
			var waiting any
			if cur != nil {
				msg = fmt.Sprintf("现在轮到 %s，请等它说完", cur.Alias)
				waiting = map[string]string{"agentId": cur.ID, "alias": cur.Alias}
			}
			return changed, domain.NewError(409, "not_your_turn", msg).With(map[string]any{"waitingFor": waiting})
		}

		text := strings.TrimSpace(in.Text)
		if text == "" {
			return changed, domain.NewError(400, "bad_request", "text 不能为空")
		}
		cost := budget.EstTokens(text)
		// §八 #12：超出单条上限直接拒，且不扣预算
		if r.MaxOutputTokens > 0 && cost > r.MaxOutputTokens {
			return changed, domain.NewError(422, "text_too_long",
				fmt.Sprintf("单条发言上限 %d est_tokens，本条 %d", r.MaxOutputTokens, cost)).
				With(map[string]any{"limit": r.MaxOutputTokens, "actual": cost})
		}

		isFinalRound := r.Round >= r.MaxRounds
		// §四：第 2..N-1 轮必须 replyTo
		if r.Round >= 2 && !isFinalRound {
			if in.ReplyTo == nil || *in.ReplyTo == 0 {
				return changed, domain.NewError(422, "reply_to_required", "交锋轮必须 replyTo 别人的一条发言")
			}
			var target *domain.Message
			for _, m := range agg.Messages {
				if m.Seq == *in.ReplyTo {
					target = m
					break
				}
			}
			if target == nil {
				return changed, domain.NewError(400, "bad_request", "replyTo 指向的发言不存在")
			}
		}
		if in.SupportProposalID != "" && agg.ProposalByID(in.SupportProposalID) == nil {
			return changed, domain.NewError(400, "bad_request", "supportProposalId 指向的方案不存在")
		}

		// 预算护栏：辩论轮可被拦；进入投票/概述的提交永不拦截
		if r.BudgetUsed+cost > budget.HardCap(r.TokenBudget) {
			e.enterVoting(agg, "budget")
			return true, domain.NewError(402, "budget_exhausted",
				"预算已到红线，会议转入投票").With(map[string]any{"budget": budgetView(r)})
		}

		r.Seq++
		msg := &domain.Message{
			TenantID: tenantID, RoomID: roomID, Seq: r.Seq, Round: r.Round,
			AgentID: a.ID, Alias: a.Alias, Model: a.Model, Text: text,
			ReplyTo: in.ReplyTo, SupportProposalID: in.SupportProposalID,
			Mentions: parseMentions(agg, text),
			TS:       time.UnixMilli(e.now()).UTC().Format("2006-01-02T15:04:05.000Z"),
			OpKey:    opKey,
		}
		agg.pushMsg(msg)
		charge(r, a, "write", cost)
		if err := tx.PutOpKey(roomID, a.ID, opKey, msg.Seq); err != nil {
			return changed, err
		}

		r.ChainTimeouts = 0
		r.RoundIdleSince = e.now() // 有人说话了 → 本轮不是"整轮无人"
		r.RoundSkipped = false     // 本轮已有内容，撤掉"空轮"标记
		e.advancePlan(agg)

		curAfter := currentTurnAgent(agg)
		yt := curAfter != nil && curAfter.ID == a.ID
		out = &PostResult{Seq: msg.Seq, Budget: budgetView(r), State: r.State,
			Billed: map[string]int{"tokens": cost}, YourTurn: &yt,
			SupportProposalID: msg.SupportProposalID}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parseMentions(agg *Agg, text string) []string {
	ids := []string{}
	for _, a := range agg.Agents {
		if strings.Contains(text, "@"+a.Alias) {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// =============================================================================
// 方案
// =============================================================================

// Propose 提交方案（第 2..N-1 轮，每人最多 2 个，提交后不可改）。
func (e *Engine) Propose(ctx context.Context, tenantID, roomID, tokenHash string, in ProposeInput) (*ProposeResult, error) {
	var out *ProposeResult
	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		r := agg.Room
		if r.State != domain.StateDebating {
			return changed, domain.NewError(403, "wrong_round", "只有讨论阶段可以提交方案")
		}
		// ★ 方案窗口 = 第 2 轮到进入投票为止（含收尾轮），不再排除最后一轮。
		//
		//   原来的写法是「第 2 到倒数第二轮」，在 10 轮房里没问题，但 3 轮房的窗口只有
		//   第 2 轮一轮；而第 2 轮里每人只有 1 条发言额度 —— **最后一个发言的人一开口，
		//   轮次就推进、窗口就关了**，他的方案永远提不出来。实测：一场 3 轮会里整整一位
		//   agent 的方案被这条规则吃掉，全场只剩一个方案，投票变成"给唯一选项盖章"。
		//   规则的本意只是"第 1 轮先亮立场、别急着上方案"，收尾轮提方案并不违背它。
		if r.Round == 1 {
			return changed, domain.NewError(403, "wrong_round",
				fmt.Sprintf("第 1 轮只亮立场，方案从第 2 轮起可以提交（本场共 %d 轮）", r.MaxRounds))
		}
		revises := strings.TrimSpace(in.Revises)
		var target *domain.Proposal
		if revises != "" {
			target = agg.ProposalByID(revises)
			if target == nil {
				return changed, domain.NewError(400, "bad_request", "revises 指向的方案不存在")
			}
			if target.AgentID != a.ID {
				return changed, domain.NewError(403, "wrong_round", "只能修订你自己提的方案")
			}
			if target.SupersededBy != "" {
				return changed, domain.NewError(409, "already_superseded",
					fmt.Sprintf("该方案已被 %s 取代，请直接修订 %s（或提一个新方案）", target.SupersededBy, target.SupersededBy))
			}
			if agg.ChainRevisions(target.ID) >= MaxRevisionsPerProposal {
				return changed, domain.NewError(409, "too_many_revisions",
					fmt.Sprintf("每个方案最多修订 %d 次（含链上所有版本）", MaxRevisionsPerProposal))
			}
		} else if agg.ProposalsBy(a.ID) >= MaxProposalsPerAgent {
			return changed, domain.NewError(409, "too_many_proposals",
				fmt.Sprintf("每人最多 %d 个方案（修订自己的方案不算配额）", MaxProposalsPerAgent))
		}
		title := strings.TrimSpace(in.Title)
		body := strings.TrimSpace(in.Body)
		if title == "" || body == "" {
			return changed, domain.NewError(400, "bad_request", "title 和 body 都不能为空")
		}
		if n := len([]rune(title)); n > 60 {
			// 错误码必须能指认"是标题超了"：两位验证 agent 都被 text_too_long 误导成正文超长，
			// 然后去压缩正文，白折腾一轮。
			return changed, domain.NewError(422, "title_too_long",
				fmt.Sprintf("title 上限 60 字，当前 %d（正文另有上限 %d est_tokens）", n, r.MaxOutputTokens)).
				With(map[string]any{"limit": 60, "actual": n})
		}
		cost := budget.EstTokens(body)
		if r.MaxOutputTokens > 0 && cost > r.MaxOutputTokens {
			return changed, domain.NewError(422, "text_too_long",
				fmt.Sprintf("方案正文上限 %d est_tokens，当前 %d", r.MaxOutputTokens, cost)).
				With(map[string]any{"limit": r.MaxOutputTokens, "actual": cost})
		}

		r.Seq++
		p := &domain.Proposal{
			TenantID: tenantID, RoomID: roomID,
			ID:      fmt.Sprintf("p_%d", len(agg.Proposals)+1),
			Seq:     r.Seq,
			AgentID: a.ID, Alias: a.Alias, Model: a.Model,
			Title: title, Body: body, CreatedAt: e.now(),
			Supersedes: revises,
		}
		agg.addProposal(p)
		if target != nil {
			// 旧版本标记为已被取代（正文不动）；用 touch 而不是 addProposal ——
			// 后者会把同一个指针再塞进列表一次，导致方案数/平票判断全被污染。
			target.SupersededBy = p.ID
			agg.touchProposal(target.ID)
			for _, v := range agg.Votes {
				if v.ProposalID == target.ID {
					v.ProposalID = p.ID
					agg.putVote(v)
					if err := tx.RetargetVotes(roomID, target.ID, p.ID); err != nil {
						return changed, err
					}
				}
			}
		}
		charge(r, a, "write", cost)
		// 注意：新建的这个版本自己**不是**被取代的一方，所以响应里只回"我取代了谁"。
		// 曾经把 SupersededBy 写成新方案自己的 ID —— 验证 agent 一眼看出"字段语义是坏的"。
		out = &ProposeResult{ProposalID: p.ID, Seq: r.Seq, Count: agg.ProposalsBy(a.ID),
			Supersedes: revises, Superseded: revises}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// =============================================================================
// 投票
// =============================================================================

// Vote 投票。二选一形态：
//   - 有方案：{proposalId}（null = 弃权）
//   - 无方案（§九 降级）：{finalStance}
//
// 混用一律 403 wrong_round —— 否则票型混乱，获胜判定没法机械算。
func (e *Engine) Vote(ctx context.Context, tenantID, roomID, tokenHash string, in VoteInput) (*VoteResult, error) {
	var out *VoteResult
	redirected := ""
	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		r := agg.Room
		if r.State != domain.StateVoting {
			// 窗口关闭后来投票的人，要明确知道"你那票已经按弃权算了"，而不是只看到
			// 一句「当前是 summarizing 阶段」——实测 Lead 亲自当 agent 时就是在这句
			// 报错上卡了半分钟，一直以为是自己参数写错了。
			msg := fmt.Sprintf("当前是 %s 阶段，不能投票", r.State)
			if r.State == domain.StateSummarizing || r.State == domain.StateEnded {
				if v := agg.VoteByAgent(a.ID); v != nil && v.ProposalID == "" {
					msg = fmt.Sprintf("投票窗口已关闭（%s），你的一票已按弃权计入，不能再改；结论按已投的票机械统计",
						VotingWindow)
				}
			}
			return changed, domain.NewError(403, "wrong_round", msg)
		}

		hasProposals := len(agg.Proposals) > 0
		if hasProposals && in.FinalStance != "" {
			return changed, domain.NewError(403, "wrong_round", "已有方案时只能用 proposalId 投票")
		}
		if !hasProposals && in.FinalStance == "" {
			return changed, domain.NewError(422, "final_stance_required", "没有方案时请提交 finalStance")
		}

		v := &domain.Vote{
			TenantID: tenantID, RoomID: roomID, AgentID: a.ID,
			Reason: strings.TrimSpace(in.Reason), CreatedAt: e.now(),
		}
		// 本人回来投票时，覆盖掉系统替他预置的那条弃权（阈值只能猜，允许自行纠正）
		if old := agg.VoteByAgent(a.ID); old != nil && old.Reason == AutoAbstainReason {
			v.Reason = strings.TrimSpace(in.Reason)
		}
		if hasProposals {
			if in.ProposalID != nil && *in.ProposalID != "" {
				prop := agg.ProposalByID(*in.ProposalID)
				if prop == nil {
					return changed, domain.NewError(400, "bad_request", "proposalId 指向的方案不存在")
				}
				// 投给了"已被修订取代"的旧版本 → 自动改投它最新的版本，并如实告知。
				// 这让"先投、后修订"和"先修订、后投"两种顺序得到同一个结果。
				// RedirectedFrom 留痕：结论里要能看见"本意是投旧版"。
				if prop.SupersededBy != "" {
					if latest := agg.LatestAdopted(prop.ID); latest != nil && latest.ID != prop.ID {
						redirected = latest.ID
						v.RedirectedFrom = prop.ID
						prop = latest
					}
				}
				// 允许投自己（禁止自投会逼"唯一认真提了方案的人"去投更差的方案）。
				// 代价用跨票拆分来化解：获胜判定按 cross 票排，自投单独计数、不单独构成多数。
				v.ProposalID = prop.ID
			}
		} else {
			st := in.FinalStance
			if st != domain.StanceAgree && st != domain.StanceDisagree && st != domain.StanceAbstain {
				return changed, domain.NewError(400, "bad_request", "finalStance 只能是 agree/disagree/abstain")
			}
			v.FinalStance = st
			a.FinalStance = st
		}
		agg.putVote(v)

		// 全员投完（含弃权）→ 走统一结算判定（含 vote-grace 宽限期，别在这里另写一份）
		e.maybeSettleVoting(agg)
		out = &VoteResult{Votes: TallyVotes(agg), Cast: len(agg.Votes), State: r.State,
			RedirectedTo: redirected}
		if !hasProposals {
			out.Votes = StanceTally(agg)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// =============================================================================
// 概述
// =============================================================================

// Summary 由获胜者撰写概述（§五 / §八 #10 #11）。
func (e *Engine) Summary(ctx context.Context, tenantID, roomID, tokenHash string, in SummaryInput) (*SummaryResult, error) {
	var out *SummaryResult
	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		a := agg.AgentByToken(tokenHash)
		if a == nil {
			return changed, domain.ErrNotFound
		}
		a.LastSeenAt = e.now() // 刷新活跃时间（reclaim 防双开用）
		r := agg.Room

		// ★ 概述已经存在：必须分清"这是你自己那份"还是"你来不及、系统兜底的"。
		//
		//   原来的写法一律回 {"ok":true}。实测（Lead 亲自当 agent）：超时 85 秒后才把
		//   精心写的概述 POST 上来，拿到 200 ok:true —— 而 result 里躺着的是机械摘要。
		//   agent 会理直气壮地告诉主人"我写了概述"，主人因此看不到它真正想说的那句
		//   「四张票全是自投，别当共识」。**一个说谎的 200 比一个诚实的 409 更贵。**
		if r.SummaryText != "" {
			mine := strings.TrimSpace(in.Text) == strings.TrimSpace(r.SummaryText)
			out = &SummaryResult{OK: mine, Saved: mine, State: r.State, Author: r.SummaryAuthor}
			if mine {
				out.Reason = "already_saved"
				out.Note = "你之前提交的概述已经在结果里，这是幂等重放，没有重复写入。"
			} else {
				out.Reason = "summary_too_late"
				out.Note = fmt.Sprintf("你的概述没有被采用：会议已于 %s 截止，结果里用的是「%s」。请直接读 /result 回报主人，并在回报里说明概述不是你写的。",
					e.summaryTO, r.SummaryAuthor)
			}
			return changed, nil
		}
		if r.State != domain.StateSummarizing {
			return changed, domain.NewError(403, "wrong_round", fmt.Sprintf("当前是 %s 阶段，不能写概述", r.State))
		}

		author := e.summaryAuthor(agg)
		if author == nil || author.ID != a.ID {
			return changed, domain.NewError(403, "wrong_round", "只有获胜方案的提交者可以撰写概述")
		}

		text := strings.TrimSpace(in.Text)
		if text == "" {
			return changed, domain.NewError(400, "bad_request", "text 不能为空")
		}
		cost := budget.EstTokens(text)
		if r.MaxOutputTokens > 0 && cost > r.MaxOutputTokens {
			return changed, domain.NewError(422, "text_too_long",
				fmt.Sprintf("概述上限 %d est_tokens，当前 %d", r.MaxOutputTokens, cost)).
				With(map[string]any{"limit": r.MaxOutputTokens, "actual": cost})
		}

		// 概述永不被预算拦截（5% 预留就是给它的）
		charge(r, a, "write", cost)
		r.SummaryText = text
		r.SummaryAuthor = a.Alias
		r.SummaryAt = e.now()
		e.endRoom(agg, "summary_submitted")
		out = &SummaryResult{OK: true, Saved: true, State: r.State, Author: r.SummaryAuthor}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// summaryAuthor 决定谁该写概述：获胜方案提交者；无方案时是主席。
func (e *Engine) summaryAuthor(agg *Agg) *domain.Agent {
	if w := TallyWinner(agg); w != nil {
		return agg.AgentByID(w.AgentID)
	}
	if mod := agg.Moderator(); mod != nil {
		return mod
	}
	if len(agg.Agents) == 1 {
		return agg.Agents[0]
	}
	return nil
}

// =============================================================================
// 其他
// =============================================================================

// Close 房主提前结束（§八 #19：有方案进投票，否则直接结束）。
func (e *Engine) Close(ctx context.Context, tenantID, roomID, adminKeyHash string) (*domain.Room, error) {
	var out *domain.Room
	err := e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		r := agg.Room
		if adminKeyHash == "" || adminKeyHash != r.AdminKeyHash {
			return changed, domain.ErrNotFound
		}
		if r.State != domain.StateEnded {
			if len(agg.Proposals) > 0 && r.State == domain.StateDebating {
				e.enterVoting(agg, "admin_close")
			} else if r.State == domain.StateDebating || r.State == domain.StateBriefing {
				e.endRoom(agg, "admin_close")
			}
			changed = true
		}
		cp := *r
		out = &cp
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// View 只读访问（Meta / Result / 页面渲染用），内部仍会 tick。
func (e *Engine) View(ctx context.Context, tenantID, roomID string, fn func(agg *Agg) error) error {
	return e.Do(ctx, tenantID, roomID, func(agg *Agg, tx *store.Tx) (bool, error) {
		changed := e.tick(agg)
		if err := fn(agg); err != nil {
			return changed, err
		}
		return changed, nil
	})
}

// healthOf 计算会议健康度：多久没动静、是否已停摆（超过 2×回合超时无任何消息）。
func (e *Engine) healthOf(agg *Agg) Health { return healthAt(agg, e.now(), e.turnTO) }

// healthAt 是 healthOf 的纯函数版本：时间与回合超时都由调用方给。
// 离线验签用它，保证不读真实时钟（docs/09 §6）。
func healthAt(agg *Agg, now int64, turnTO time.Duration) Health {
	r := agg.Room
	last := r.CreatedAt
	for _, m := range agg.Messages {
		if t, err := time.Parse("2006-01-02T15:04:05.000Z", m.TS); err == nil {
			if ms := t.UnixMilli(); ms > last {
				last = ms
			}
		}
	}
	if r.StartedAt > last {
		last = r.StartedAt
	}
	sec := int((now - last) / 1000)
	if sec < 0 {
		sec = 0
	}
	stalled := r.State != domain.StateEnded && int64(sec)*1000 > 2*turnTO.Milliseconds()
	return Health{LastActivityInSec: sec, Stalled: stalled}
}

// ResultFor 返回结论（已结束用冻结快照，快照版本过旧则按当前代码重算）。
func (e *Engine) ResultFor(agg *Agg) *Result {
	r := agg.Room
	if r.State == domain.StateEnded && r.ResultJSON != "" {
		var res Result
		if err := json.Unmarshal([]byte(r.ResultJSON), &res); err == nil {
			// ★ 只信任"同一契约版本"的冻结快照。
			//   升级实测：v2.4 冻结的旧房间快照里没有 decisionSet，/result 直接返回旧形状 ——
			//   对下游来说，同一个接口在升级前后有两种 schema，比慢一点糟得多。
			//   版本不一致就重算（结论是确定性的，重算结果与冻结时一致，只是多了新字段）。
			if res.SchemaVersion == ResultSchemaVersion {
				res.Health = e.healthOf(agg)
				return &res
			}
		}
	}
	res := buildResult(agg)
	res.Health = e.healthOf(agg)
	return res
}

// =============================================================================
// 宿主专用切口（docs/09 §3/§4）
// =============================================================================

// PeekReadOnly 只读地载入一次聚合，**不 tick、不落库**（arch R-7）。
//
// 为什么单独开一个入口：View 内部会 tick，而 tick 会因为"时间到点"替别人弃权、
// 推进轮次。本地宿主上任何人发一个无凭证 GET 就能替别人弃权，这是 R-7 明令禁止的。
// 宿主的所有只读端点（result / status / head）都必须走这里。
func (e *Engine) PeekReadOnly(ctx context.Context, tenantID, roomID string, fn func(agg *Agg) error) error {
	return e.peek(ctx, tenantID, roomID, false, fn)
}

// peek 是只读读取的公共骨架：走只读事务、按 rt 锁串行，不 tick、不落库。
func (e *Engine) peek(ctx context.Context, tenantID, roomID string, light bool, fn func(agg *Agg) error) error {
	rt := e.runtimeFor(tenantID, roomID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return e.store.ReadOnly(ctx, tenantID, func(tx *store.Tx) error {
		var (
			agg *Agg
			err error
		)
		if light {
			agg, err = loadAggLight(tx, roomID)
		} else {
			agg, err = loadAgg(tx, roomID)
		}
		if err != nil {
			return err
		}
		return fn(agg)
	})
}

// PeekLight 与 PeekReadOnly 相同，但**不读消息正文与方案正文**：
// 给"只要状态与计数"的路径用（宿主的快照刷新）。
func (e *Engine) PeekLight(ctx context.Context, tenantID, roomID string, fn func(agg *Agg) error) error {
	return e.peek(ctx, tenantID, roomID, true, fn)
}

// StatusSnapshot 是状态页要用的轻量数据：没有消息正文，也没有方案全文。
//
// 为什么单独一条路径：状态页是最高频的读，而"完整载入聚合"要把所有消息读进内存，
// 6 人 10 轮之后光解析就占掉大部分时间与分配。
type StatusSnapshot struct {
	Room   *domain.Room
	Agents []*domain.Agent
	Votes  []*domain.Vote
	Counts store.RoomCounts
}

// PeekStatus 只读、不 tick、不落库，读的量刚好够画状态页。
func (e *Engine) PeekStatus(ctx context.Context, tenantID, roomID string) (*StatusSnapshot, error) {
	var out *StatusSnapshot
	err := e.store.ReadOnly(ctx, tenantID, func(tx *store.Tx) error {
		r, err := tx.GetRoom(roomID)
		if err != nil {
			return err
		}
		agents, err := tx.ListAgents(roomID)
		if err != nil {
			return err
		}
		votes, err := tx.ListVotes(roomID)
		if err != nil {
			return err
		}
		counts, err := tx.RoomCounts(roomID)
		if err != nil {
			return err
		}
		out = &StatusSnapshot{Room: r, Agents: agents, Votes: votes, Counts: counts}
		return nil
	})
	return out, err
}

// BuildResultForVerify 是给离线验签用的**纯聚合出口**（arch B-12 / docs/09 §6）。
//
// 它不读库、不读真实时钟、不生成任何新 ID：只把一份重放出来的 Agg 聚合成 Result。
// now 只用于 endedAt 的兜底（未结束房间的 minutes 计算），不参与 decisionHash 域。
func BuildResultForVerify(agg *Agg, now int64) *Result {
	res := buildResultAt(agg, now)
	// Health 不在 decisionHash 域内，但也不许读真实时钟：用传入的 now 推。
	res.Health = healthAt(agg, now, 90*time.Second)
	return res
}
