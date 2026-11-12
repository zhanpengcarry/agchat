package verify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agchat/internal/domain"
	"agchat/internal/oplog"
	"agchat/internal/room"
)

// =============================================================================
// ② 从 op 流纯函数重建 room.Agg（B-12 / B-13 / B-14）
//
// 这里做的事：
//   - 只读 op 的字段与载荷，**不调用** room.Engine 的 Create/Join/Post/...，
//     不生成任何新 ID / token / 时间戳；
//   - 不读真实时钟：所有时间都来自 op 的 t 字段与 `end` 快照；
//   - 终态 Room 事实取 `end` op 携带的快照（B-14），不靠"跑一遍引擎"。
//
// 载荷字段名以 docs/09 §4.1（v1.1 冻结）为准；额外的兼容键只是为了不把
// "宿主写错一个键名"误判成伪造 —— 判据仍然是两条指纹。
// =============================================================================

// replayBlock 是"无法重放"的结构性原因（→ unverifiable，不是 failed）。
type replayBlock struct {
	code   string
	reason string
	failAt int
}

func badOp(o oplog.Op, code, reason string) *replayBlock {
	return &replayBlock{code: code, reason: reason, failAt: o.I}
}

type replayResult struct {
	agg *room.Agg
	// genesis 是签过名的 create 载荷（manifest 是打包者写的文件，谁都能改）
	genesis *createPayload
	now     int64
}

// advanceReasonKnown 判定 advance 的 reason 是否落在 docs/09 §10.2 第 6 条的
// 冻结闭枚里。判据只有一份实现（oplog.KnownReason），本包不另抄一张表 ——
// 两张表一定会分叉（arch A-25 的教训）。出现枚举外的 reason，说明证据是另一版
// 规则写出来的（或宿主自造了语义）：那属于"无从解释"，判 unverifiable，不许猜。

// replayBundle 重建 Agg。第二个返回值非空 = 无法重放（unverifiable）。
func replayBundle(ops []oplog.Op) (*replayResult, *replayBlock) {
	first := ops[0]
	r := &domain.Room{
		ID:        "replay", // 仅作占位：decisionHash 域内不含房间 ID
		State:     domain.StateBriefing,
		CreatedAt: first.T,
	}
	agg := &room.Agg{Room: r}
	var snap *roomSnapshot
	var genesis *createPayload
	var lastT int64

	for _, o := range ops {
		if o.T > lastT {
			lastT = o.T
		}
		switch o.O {
		case "create":
			var p createPayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "create 载荷解析失败："+err.Error())
			}
			if genesis == nil {
				genesis = &p
			}
			r.Topic = p.Topic
			r.MaxRounds = p.MaxRounds
			r.MaxMinutes = p.MaxMinutes
			r.MaxAgents = p.MaxAgents
			if r.MaxAgents == 0 {
				r.MaxAgents = p.Seats
			}
			r.MaxOutputTokens = p.MaxOutput
			r.TokenBudget = p.TokenBudget

		case "invite":
			// 发卡不改变房间聚合：邀请次数/过期只影响"还能不能进场"，
			// 而"进了场"这件事由 join op 记录。

		case "join":
			var p joinPayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "join 载荷解析失败："+err.Error())
			}
			// join 的 op.a 是 "host"（宿主分配座位并签名），座位身份来自载荷 agentId。
			seatID := p.AgentID
			if seatID == "" {
				seatID = o.A
			}
			a := &domain.Agent{
				ID:       seatID,
				Alias:    p.Alias,
				JoinedAt: o.T,
				Ord:      len(agg.Agents),
			}
			if p.Ord != nil {
				a.Ord = *p.Ord
			}
			agg.Agents = append(agg.Agents, a)
			if r.ModeratorID == "" {
				r.ModeratorID = a.ID // 主席 = 第一个进场的人（引擎语义）
			}
			if r.StartedAt == 0 {
				r.StartedAt = o.T
			}
			if r.State == domain.StateBriefing {
				r.State = domain.StateDebating
			}

		case "post":
			var p postPayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "post 载荷解析失败："+err.Error())
			}
			if blk := checkAuthor(o, p.AgentID); blk != nil {
				return nil, blk
			}
			msg := &domain.Message{
				TenantID:          "local",
				RoomID:            r.ID,
				Round:             p.Round,
				AgentID:           o.A,
				Alias:             p.Alias,
				Text:              p.Text,
				ReplyTo:           p.ReplyTo,
				SupportProposalID: p.Support,
				Seq:               p.Seq,
				TS:                messageTS(o.T),
			}
			if msg.Alias == "" {
				msg.Alias = aliasOf(agg, o.A)
			}
			agg.Messages = append(agg.Messages, msg)

		case "propose":
			var p proposePayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "propose 载荷解析失败："+err.Error())
			}
			if blk := checkAuthor(o, p.AgentID); blk != nil {
				return nil, blk
			}
			id := p.ID
			if id == "" {
				id = fmt.Sprintf("p_%d", len(agg.Proposals)+1) // 引擎的确定性编号规则
			}
			alias := p.Alias
			if alias == "" {
				alias = aliasOf(agg, o.A)
			}
			pr := &domain.Proposal{
				TenantID:   "local",
				RoomID:     r.ID,
				ID:         id,
				Seq:        p.Seq,
				AgentID:    o.A,
				Alias:      alias,
				Title:      p.Title,
				Body:       p.Body,
				CreatedAt:  o.T,
				Supersedes: p.Supersedes,
			}
			agg.Proposals = append(agg.Proposals, pr)
			if p.Supersedes != "" {
				// 与 engine 的 Propose 分支一致：旧版本标记被取代、已有票自动改记。
				if target := agg.ProposalByID(p.Supersedes); target != nil {
					target.SupersededBy = id
					for _, v := range agg.Votes {
						if v.ProposalID == p.Supersedes {
							v.ProposalID = id
						}
					}
				}
			}

		case "vote":
			var p votePayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "vote 载荷解析失败："+err.Error())
			}
			if blk := checkAuthor(o, p.AgentID); blk != nil {
				return nil, blk
			}
			v := &domain.Vote{
				TenantID:       "local",
				RoomID:         r.ID,
				AgentID:        o.A,
				FinalStance:    p.FinalStance,
				Reason:         p.Reason,
				CreatedAt:      o.T,
				RedirectedFrom: p.RedirectedFrom,
			}
			if p.ProposalID != nil {
				v.ProposalID = *p.ProposalID
			}
			// 投给"已被修订取代"的旧版 → 改记到最新版（引擎的同一条规则）：
			// 这让"先投后修订"和"先修订后投"得到同一个结论。
			if v.ProposalID != "" {
				if prop := agg.ProposalByID(v.ProposalID); prop != nil && prop.SupersededBy != "" {
					if latest := latestAdopted(agg, prop.ID); latest != "" && latest != prop.ID {
						if v.RedirectedFrom == "" {
							v.RedirectedFrom = prop.ID
						}
						v.ProposalID = latest
					}
				}
			}
			putVotePure(agg, v)
			if v.FinalStance != "" {
				if a := agg.AgentByID(o.A); a != nil {
					a.FinalStance = v.FinalStance
				}
			}

		case "summary":
			var p summaryPayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "summary 载荷解析失败："+err.Error())
			}
			if blk := checkAuthor(o, p.AgentID); blk != nil {
				return nil, blk
			}
			r.SummaryText = p.Text
			r.SummaryAuthor = p.AgentID
			if r.SummaryAuthor == "" {
				r.SummaryAuthor = o.A
			}
			r.SummaryAt = o.T

		case "advance":
			var p advancePayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "advance 载荷解析失败："+err.Error())
			}
			if !oplog.KnownReason(p.Reason) {
				return nil, badOp(o, CodeAdvanceReasonUnknown,
					fmt.Sprintf("第 %d 条 advance op 的 reason=%q 不在冻结枚举内（docs/09 §10.2）：这条时间跃迁的依据无从解释（C-7）",
						o.I, p.Reason))
			}

		case "end":
			var p endPayload
			if err := decodePayload(o.P, &p); err != nil {
				return nil, badOp(o, CodeReplayUnknownOp, "end 载荷解析失败："+err.Error())
			}
			if p.Snapshot == nil {
				return nil, badOp(o, CodeSnapshotMissing,
					fmt.Sprintf("第 %d 条 end op 没有携带终态快照（B-14）：终态事实无处可查，无法复算结论", o.I))
			}
			snap = p.Snapshot

		case "reject":
			// 被拒绝的 op 不进聚合；它本身是"我试过但被挡了"的证据（arch 4.4）。

		default:
			return nil, badOp(o, CodeReplayUnknownOp,
				fmt.Sprintf("第 %d 条 op 的类型 %q 不在 S1 冻结枚举内：证据包版本比本工具新，无法重放",
					o.I, o.O))
		}
	}

	if snap == nil {
		return nil, &replayBlock{
			code:   CodeSnapshotMissing,
			reason: "op 流里没有任何携带终态快照的 end op（B-14）：缺快照只能靠 tier 2 时序复算，S1 不承诺",
		}
	}
	if snap.State == "" {
		return nil, &replayBlock{
			code:   CodeSnapshotNotEnded,
			reason: "终态快照缺 state 字段：无法确认这场会已经结束（C-7：不猜）",
			failAt: endOpIndex(ops),
		}
	}
	r.State = domain.State(snap.State)
	if r.State != domain.StateEnded {
		return nil, &replayBlock{
			code:   CodeSnapshotNotEnded,
			reason: fmt.Sprintf("终态快照的 state=%q 不是 ended：bundle 不是一场已结束的会议，没有结论可比", snap.State),
			failAt: endOpIndex(ops),
		}
	}
	applySnapshot(r, snap)

	now := r.EndedAt
	if now <= 0 {
		now = lastT // 兜底：用最后一条 op 的生效时刻，绝不读墙钟
	}
	return &replayResult{agg: agg, now: now, genesis: genesis}, nil
}

// applySnapshot 把 end op 的终态快照落到 Room 上（B-14 的显式字段集）。
func applySnapshot(r *domain.Room, s *roomSnapshot) {
	r.State = domain.State(s.State)
	r.Round = s.Round
	r.EmptyRounds = s.EmptyRounds
	r.RoundSkipped = s.RoundSkipped
	r.EndReason = s.EndReason
	r.ConvergenceReason = s.ConvergenceReason
	r.WinnerProposalID = s.WinnerProposalID
	r.PhaseStartedAt = int64(s.PhaseStartedAt)
	if s.StartedAt != 0 {
		r.StartedAt = int64(s.StartedAt)
	}
	r.EndedAt = int64(s.EndedAt)
	r.Seq = s.Seq
	r.Plan = &domain.Plan{Skipped: s.PlanSkipped}
	if s.SummaryText != "" {
		r.SummaryText = s.SummaryText
	}
	if s.SummaryAuthor != "" {
		r.SummaryAuthor = s.SummaryAuthor
	}
	if s.SummaryAt != 0 {
		r.SummaryAt = int64(s.SummaryAt)
	}
}

// checkAuthor 校验载荷里的 agentId 与 op 的作者座位是否一致。
//
// 为什么要这条：op.a 是被签名覆盖的"作者座位"，载荷里的 agentId 若与之不符，
// 说明两处记录打架 —— 这种证据不能当"一致"（C-7）。
func checkAuthor(o oplog.Op, payloadAgent string) *replayBlock {
	if payloadAgent == "" || o.A == "" || payloadAgent == o.A {
		return nil
	}
	return badOp(o, CodeAuthorMismatch,
		fmt.Sprintf("第 %d 条 op 的作者座位是 %q，载荷里的 agentId 却是 %q：两处记录互相矛盾",
			o.I, o.A, payloadAgent))
}

// putVotePure 是 Agg.putVote 的纯等价物：同一座位只留一票（后来的覆盖先前的）。
func putVotePure(agg *room.Agg, v *domain.Vote) {
	for i, old := range agg.Votes {
		if old.AgentID == v.AgentID {
			agg.Votes[i] = v
			return
		}
	}
	agg.Votes = append(agg.Votes, v)
}

// latestAdopted 沿 SupersededBy 链走到"当前采纳中"的版本。
func latestAdopted(agg *room.Agg, id string) string {
	cur := id
	for i := 0; i < len(agg.Proposals)+1; i++ {
		p := agg.ProposalByID(cur)
		if p == nil || p.SupersededBy == "" {
			return cur
		}
		cur = p.SupersededBy
	}
	return cur
}

// aliasOf 从重建出的座位表里取展示名（载荷缺 alias 时的兜底，不是主路径）。
func aliasOf(agg *room.Agg, seat string) string {
	if a := agg.AgentByID(seat); a != nil {
		return a.Alias
	}
	return ""
}

// transcriptOf 用 oplog 的**唯一一份**规范编码算转写指纹（C-1）。
//
// 投影是 {i,alias,round,text,replyTo,support}（+ seq，见 §10.2 第 5 条）。
// alias 取 op 载荷自带的（v1.1 起 post 必须带 alias）；缺了才回查座位表，
// 绝不为了拿 alias 去调 room 的聚合（B-13）。
func transcriptOf(ops []oplog.Op, agg *room.Agg) (string, error) {
	msgs := make([]oplog.TranscriptMsg, 0, len(ops))
	for _, o := range ops {
		if o.O != "post" {
			continue
		}
		var p postPayload
		if err := decodePayload(o.P, &p); err != nil {
			return "", fmt.Errorf("第 %d 条 post 载荷解析失败：%w", o.I, err)
		}
		alias := p.Alias
		if alias == "" {
			alias = aliasOf(agg, o.A)
		}
		msgs = append(msgs, oplog.TranscriptMsg{
			I:       o.I,
			Seq:     p.Seq,
			Alias:   alias,
			Round:   p.Round,
			Text:    p.Text,
			ReplyTo: p.ReplyTo,
			Support: p.Support,
		})
	}
	return oplog.TranscriptHash(msgs)
}

// messageTS 把 op 的生效时刻（毫秒）转成引擎使用的消息时间格式。
// 只做格式转换，不读时钟 —— 时间源永远是 op 自己。
func messageTS(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// =============================================================================
// 载荷结构（docs/09 §4.1，v1.1 冻结；键名不许自造）
// =============================================================================

type createPayload struct {
	Ref         string      `json:"ref"`
	Key         string      `json:"key"`
	Topic       string      `json:"topic"`
	Seats       int         `json:"seats"`
	MaxRounds   int         `json:"maxRounds"`
	MaxAgents   int         `json:"maxAgents"`
	MaxMinutes  int         `json:"maxMinutes"`
	MaxOutput   int         `json:"maxOutput"`
	TokenBudget int         `json:"tokenBudget"`
	IDs         []inviteRef `json:"ids"`
	RV          *rvTriple   `json:"rv"`
	HostName    string      `json:"hn"`
	OwnerName   string      `json:"on"`
}

type inviteRef struct {
	ID   string `json:"id"`
	Uses int    `json:"uses"`
	Exp  int64  `json:"exp"`
}

// rvTriple 是规则版本三元组；值可能是数字（卡里 res:1）也可能是字符串，
// 用 flexStr 两种都收，避免"宿主写法不同"被当成伪造。
type rvTriple struct {
	Res  flexStr `json:"res"`
	Vote flexStr `json:"vote"`
	Hash flexStr `json:"hash"`
}

type joinPayload struct {
	InviteID string `json:"inviteId"`
	AgentPub string `json:"agentPub"`
	AgentID  string `json:"agentId"`
	Alias    string `json:"alias"`
	Ord      *int   `json:"ord"`
	JoinKey  string `json:"joinKey"`
}

type postPayload struct {
	Seq     int    `json:"seq"`
	Round   int    `json:"round"`
	AgentID string `json:"agentId"`
	Alias   string `json:"alias"`
	Text    string `json:"text"`
	ReplyTo *int   `json:"replyTo"`
	Support string `json:"support"`
}

type proposePayload struct {
	ID         string `json:"id"`
	Seq        int    `json:"seq"`
	AgentID    string `json:"agentId"`
	Alias      string `json:"alias"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Supersedes string `json:"supersedes"`
}

type votePayload struct {
	AgentID        string  `json:"agentId"`
	Alias          string  `json:"alias"`
	ProposalID     *string `json:"proposalId"`
	FinalStance    string  `json:"finalStance"`
	Reason         string  `json:"reason"`
	RedirectedFrom string  `json:"redirectedFrom"`
}

type summaryPayload struct {
	AgentID string `json:"agentId"`
	Alias   string `json:"alias"`
	Text    string `json:"text"`
}

type advancePayload struct {
	Reason        string         `json:"reason"`
	DeadlineHit   bool           `json:"deadlineHit"`
	AllVoted      bool           `json:"allVoted"`
	VotesSeen     []string       `json:"votesSeen"`
	SeatLastOpSeq map[string]int `json:"seatLastOpSeq"`
}

type endPayload struct {
	Snapshot *roomSnapshot `json:"snapshot"`
	Snap     *roomSnapshot `json:"snap"` // 兼容键，仅为了不误判
}

// roomSnapshot 是 end op 携带的终态快照（B-14 显式枚举字段集）。
type roomSnapshot struct {
	State             string   `json:"state"`
	Round             int      `json:"round"`
	EmptyRounds       int      `json:"emptyRounds"`
	RoundSkipped      bool     `json:"roundSkipped"`
	EndReason         string   `json:"endReason"`
	ConvergenceReason string   `json:"convergenceReason"`
	WinnerProposalID  string   `json:"winnerProposalId"`
	PhaseStartedAt    msValue  `json:"phaseStartedAt"`
	StartedAt         msValue  `json:"startedAt"`
	EndedAt           msValue  `json:"endedAt"`
	Seq               int      `json:"seq"`
	PlanSkipped       []string `json:"planSkipped"`
	SummaryText       string   `json:"summaryText"`
	SummaryAuthor     string   `json:"summaryAuthor"`
	SummaryAt         msValue  `json:"summaryAt"`
}

// decodePayload 解 op 载荷。空载荷/`null` 视为空对象（载荷结构由 op 类型决定）。
func decodePayload(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// msValue 是毫秒时间戳：接受 JSON 数字、数字字符串或 RFC3339 字符串。
// （v1.1 schema 是数字；宽容只是为了不把宿主的时间写法差异误判成伪造。）
type msValue int64

func (m *msValue) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*m = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*m = 0
			return nil
		}
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			*m = msValue(n)
			return nil
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z"} {
			if t, err := time.Parse(layout, str); err == nil {
				*m = msValue(t.UnixMilli())
				return nil
			}
		}
		return fmt.Errorf("verify: 无法解析时间 %q", str)
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*m = msValue(n)
	return nil
}

// flexStr 接受字符串或数字（规则版本三元组两种写法都有）。
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = ""
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*f = flexStr(str)
		return nil
	}
	*f = flexStr(s)
	return nil
}
