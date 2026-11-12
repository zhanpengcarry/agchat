package room

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"agchat/internal/domain"
)

// =============================================================================
// 产物：全部机械聚合，零模型调用
// =============================================================================

// DecisionSet 是给机器读的结构化结论（v2.5）。
//
// 为什么要有它：ownerReport 是一段人读的中文，下游接不了工单、进不了审批；
// 而「结论可核对」又要求每条结论都能指回票数与修订记录。于是同一份机械聚合
// 输出两份：人读 ownerReport、机读 decision_set，两者由同一个函数生成，不会打架。
type DecisionSet struct {
	SchemaVersion string `json:"schemaVersion"`
	Status        string `json:"status"` // decided | majority | split | tie | no_consensus | no_options
	Topic         string `json:"topic"`

	Chosen  *DecisionOption  `json:"chosen"`
	Options []DecisionOption `json:"options"`

	// DeclaredSupport 记录"谁在收尾轮声明支持谁"，与 Votes 里的实投分开。
	DeclaredSupport []DeclaredSupport `json:"declaredSupport"`
	// Revisions 记录方案修订链（旧版本→新版本），正文不可改但可以取代。
	Revisions []RevisionRecord `json:"revisions"`
	// RedirectedVotes 记录"本意投旧版、被自动改记到新版"的票（留痕，不然结论里看不见）。
	RedirectedVotes []VoteRedirect `json:"redirectedVotes"`

	Participation Participation `json:"participation"`
	// Disagreements 是**真分歧**：有人没投票、声明≠实投、平票破平、没有任何方案。
	// 语义分歧仍只能读 summary.text（这里给不了，也不假装给得了）。
	// **只有会议结束后才有值**：会中的半截票数写进来只会误导。
	Disagreements []string `json:"disagreements"`
	// Facts 是**机械事实**（发生了 N 次修订、谁的票被改记），不是分歧。
	// 与 Disagreements 分开：验证 agent 指出把"票被改记"混进 disagreements，
	// 会让"跟结论对不对"的判断被一堆正常事件淹没（6 条里 4 条是改记）。
	Facts []string `json:"facts"`

	// Hash 是结论指纹：同一输入必得同一 hash，用来跨场对比/回归/审计。
	// 只有 ended 才有值（会中省略，而不是给空串——空串会被误读成"指纹可用"）。
	Hash string `json:"hash,omitempty"`
}

// DecisionOption 是一个候选方案在结论里的形态。
type DecisionOption struct {
	ProposalID string    `json:"proposalId"`
	Title      string    `json:"title"`
	OwnerAlias string    `json:"ownerAlias"`
	Votes      VoteCount `json:"votes"`
	// Active=true 表示它是**修订链上的最新版本**（还能被投），不代表它赢了 ——
	// 用 chosen 判断谁赢。字段名刻意不叫 adopted：验证 agent 一眼指出"adopted:true 有三个、
	// 与 chosen 冲突"，那是名字的锅，不是逻辑的锅。
	Active bool `json:"active"`
	// Winner=true 只有一个：就是 conclusion.winner。
	Winner bool `json:"winner"`
	// Revisions 是**整条修订链**上一共有几个修订版（链上每个版本都显示同一个数）。
	// 用"链上的修订次数"而不是"直接子版本数"：当选的通常是链尾（子版本数=0），
	// 只显示 0 会让人误以为"这个方案没被改过"。字段名带上 chain 以免被当成本节点计数。
	ChainRevisions int `json:"chainRevisions"`
}

// VoteRedirect 是一张"本意投旧版、被改记到新版"的票。
type VoteRedirect struct {
	Alias string `json:"alias"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// DeclaredSupport 是"声明支持 vs 实际投票"的一行。
type DeclaredSupport struct {
	Alias           string  `json:"alias"`
	DeclaredSupport *string `json:"declaredSupport"`
	VotedFor        *string `json:"votedFor"`
	// DeclaredResolved = declaredSupport 沿着修订链解析到"当前采纳中"的版本。
	// 用途：声明的是已被取代的旧版时，直接拿 declaredSupport 和 votedFor 比会误判"说一套投一套"。
	DeclaredResolved *string `json:"declaredResolved"`
}

// RevisionRecord 是一条修订记录。
type RevisionRecord struct {
	From      string `json:"from"`
	To        string `json:"to"`
	By        string `json:"by"`
	AtRound   int    `json:"atRound,omitempty"`
	FromTitle string `json:"fromTitle"`
	ToTitle   string `json:"toTitle"`
}

// Participation 是参与度事实（用来做"结论代表性"判断，也用来跨场做基线）。
type Participation struct {
	Members   int `json:"members"`
	Voted     int `json:"voted"`
	Abstained int `json:"abstained"`
	Declared  int `json:"declared"` // 有 declaredSupport 的人数
	Rounds    int `json:"rounds"`
	Messages  int `json:"messages"`
	EstTokens int `json:"estTokens"`
	Minutes   int `json:"minutes"`
	// Proposals 是**独立方案数**（不含修订版）——与"每人 2 个"的配额口径一致。
	Proposals int `json:"proposals"`
	// ProposalVersions 是方案记录总数（含每个修订版）。两者分开：
	// 验证 agent 报过"proposals=9 但独立方案其实 3 个"，一个字段装两种口径必然被误读。
	ProposalVersions int `json:"proposalVersions"`
}

// WinnerInfo 是获胜方案及其提交者。
type WinnerInfo struct {
	ProposalID string `json:"proposalId"`
	Title      string `json:"title"`
	OwnerAlias string `json:"ownerAlias"`
	OwnerModel string `json:"ownerModel"`
}

// Conclusion 是结论。
type Conclusion struct {
	Text       string               `json:"text"`
	Method     string               `json:"method"` // vote（方案票） | stance（v1 降级）
	Winner     *WinnerInfo          `json:"winner"`
	Votes      map[string]VoteCount `json:"votes"`      // total/self/cross 拆分
	VoteTotals map[string]int       `json:"voteTotals"` // 扁平总票数（老消费者用）
	Agreement  string               `json:"agreement"`  // unanimous | majority | split | tie | no_consensus
	// TieBreak 只在 agreement=tie 时有值，说明**到底是哪条规则**把并列最高票里的那个
	// 方案记成了 winner：moderator（主席票优先）| earliest（最早提交）。
	// ★ 必须如实写：原来无论实际用了哪条，文案都写"按主席优先规则"——而主席根本没投票。
	TieBreak string `json:"tieBreak,omitempty"`
}

// Section 是总结里的一段。
type Section struct {
	H string `json:"h"`
	P string `json:"p"`
}

// SummaryBlock 是概述。
type SummaryBlock struct {
	Text     string    `json:"text"`
	Author   string    `json:"author"`
	Sections []Section `json:"sections"`
}

// ProposalView 是结果里的方案。
type ProposalView struct {
	ProposalID string `json:"proposalId"`
	Title      string `json:"title"`
	// Body 只有 /result 会给；inbox 里刻意留空并省略这个字段（见 proposalViews 的注释）。
	Body  string `json:"body,omitempty"`
	By    string `json:"by"`
	Votes int    `json:"votes"`
	// 修订链：Supersedes 非空 = 它是某个方案的修订版；SupersededBy 非空 = 它已被新版取代
	// （正文仍保留可追溯，但不再参与投票与获胜判定）。
	Supersedes   string `json:"supersedes,omitempty"`
	SupersededBy string `json:"supersededBy,omitempty"`
}

// Position 是立场矩阵的一行。
type Position struct {
	Alias    string  `json:"alias"`
	Model    string  `json:"model"`
	Summary  string  `json:"summary"`
	VotedFor *string `json:"votedFor"` // null = 弃权
	// DeclaredSupport 是它在发言里**声明**支持的方案（收尾轮 supportProposalId），
	// null = 没声明过。
	//
	// ★ 为什么要和 VotedFor 分开：验证 agent 指出「结果页无法区分『说要投谁』和『实际投了谁』」——
	//   这是结论可核对的关键一环：一个人第 4 轮说支持 p_2、最后却投了 p_3，必须看得出来。
	DeclaredSupport *string `json:"declaredSupport"`
	// KeyPoints 是显式数组形式（summary 是它的第一条）。
	// 不变量：只要这个 agent 发过 ≥1 条消息，KeyPoints 就非空 —— 不许留空数组。
	KeyPoints []string `json:"keyPoints"`
}

// VoteCount 是某个方案的票数拆分：自投 vs 跨票。
//
// 为什么必须拆：允许自投之后，"总票数"会失真 —— 3 个人各提一个方案、各投自己，
// 每个方案都是 1 票，看起来势均力敌，其实没有一个人认可别人的方案。
// 跨票（cross）才是"别人觉得对不对"的真实信号。
type VoteCount struct {
	Total int `json:"total"`
	Self  int `json:"self"`
	Cross int `json:"cross"`
}

// TimelineItem 是时间线。
type TimelineItem struct {
	Seq   int    `json:"seq"`
	Round int    `json:"round"`
	Alias string `json:"alias"`
	Gist  string `json:"gist"`
}

// Cost 是成本统计。
type Cost struct {
	EstTokens int     `json:"estTokens"`
	Messages  int     `json:"messages"`
	Rounds    int     `json:"rounds"`
	Minutes   float64 `json:"minutes"`
}

// Health 是给人看的会议健康度。
type Health struct {
	LastActivityInSec int  `json:"lastActivityInSec"`
	Stalled           bool `json:"stalled"`
}

// ResultSchemaVersion 是 /result 契约的版本号。
//
// ★ 作用不是"文档好看"，而是让**冻结快照**能被安全地识别：房间结束时会把结论序列化进
// rooms.result_json，之后 /result 默认读那份快照。升级到新版本后，旧房间的快照里
// 没有新字段（比如 decisionSet）——没有版本号就只能二选一：要么永远返回旧形状，
// 要么无条件丢弃快照。有了它：版本一致走快照，不一致就按当前代码重算。
const ResultSchemaVersion = "2"

// Result 是 §七 的对外结果契约。
type Result struct {
	// SchemaVersion 见上面的常量说明；缺这个字段的老快照会被判定为"需要重算"。
	SchemaVersion string         `json:"schemaVersion"`
	State         domain.State   `json:"state"`
	RoomNo        string         `json:"roomNo"`
	Topic         string         `json:"topic"`
	Conclusion    Conclusion     `json:"conclusion"`
	Summary       SummaryBlock   `json:"summary"`
	Proposals     []ProposalView `json:"proposals"`
	Positions     []Position     `json:"positions"`
	Timeline      []TimelineItem `json:"timeline"`
	Health        Health         `json:"health"`
	OwnerReport   string         `json:"ownerReport"`
	Cost          Cost           `json:"cost"`
	// DecisionSet 是同一份机械聚合的**机读**形态（含结论指纹），见类型注释。
	DecisionSet *DecisionSet `json:"decisionSet,omitempty"`
}

// =============================================================================
// 计票与获胜判定（§五，纯机械）
// =============================================================================

// TallyVotes 返回每个方案的票数，外加 "abstain"。
func TallyVotes(agg *Agg) map[string]int {
	out := map[string]int{"abstain": 0}
	for _, p := range agg.Proposals {
		out[p.ID] = 0
	}
	for _, v := range agg.Votes {
		if v.ProposalID == "" {
			out["abstain"]++
			continue
		}
		if _, ok := out[v.ProposalID]; ok {
			out[v.ProposalID]++
		}
	}
	return out
}

// TallyDetail 返回每个方案的 total/self/cross 拆分（自投 vs 跨票）。
func TallyDetail(agg *Agg) map[string]VoteCount {
	out := map[string]VoteCount{}
	for _, p := range agg.Proposals {
		out[p.ID] = VoteCount{}
	}
	for _, v := range agg.Votes {
		if v.ProposalID == "" {
			continue
		}
		c, ok := out[v.ProposalID]
		if !ok {
			continue
		}
		c.Total++
		if p := agg.ProposalByID(v.ProposalID); p != nil && p.AgentID == v.AgentID {
			c.Self++
		} else {
			c.Cross++
		}
		out[v.ProposalID] = c
	}
	return out
}

// flatVotes 把拆分票摊平，给只关心总票数的消费者。
//
// ★ abstain 是**恒定键**：没人弃权时也必须是 0，不能缺字段。
//
//	一个字段"有时存在有时不存在"就是陷阱 —— 消费者读到 nil 和读到 0 分不清，
//	今天已经绊了两个人（eng-scout 的用例、和一位干净 agent）。
func flatVotes(v map[string]VoteCount) map[string]int {
	out := make(map[string]int, len(v)+1)
	for k, c := range v {
		out[k] = c.Total
	}
	if _, ok := out["abstain"]; !ok {
		out["abstain"] = 0
	}
	return out
}

// TallyWinner 机械判定获胜方案（§五 三级判定）：
//
//  1. 票数最多者胜
//  2. 平票时：主席投的那个胜
//  3. 主席没投 / 也平 → 最早提交的方案胜
//  4. 无任何方案 → nil
func TallyWinner(agg *Agg) *domain.Proposal {
	adopted := agg.AdoptedProposals()
	if len(adopted) == 0 {
		return nil
	}
	detail := TallyDetail(agg)

	// ★ 以**跨票**为主序：cross 才是"别人觉得这个方案对不对"的真实信号。
	//   自投只作为次序键，且不能单独构成多数（见 buildResult 里的多数判定）。
	rank := func(p *domain.Proposal) (int, int) { return detail[p.ID].Cross, detail[p.ID].Total }

	bestCross, bestTotal := -1, -1
	var tied []*domain.Proposal
	for _, p := range adopted {
		c, t := rank(p)
		switch {
		case c > bestCross || (c == bestCross && t > bestTotal):
			bestCross, bestTotal = c, t
			tied = []*domain.Proposal{p}
		case c == bestCross && t == bestTotal:
			tied = append(tied, p)
		}
	}
	if len(tied) == 0 || (bestCross == 0 && bestTotal == 0) {
		return nil // 全员弃权
	}
	if len(tied) == 1 {
		return tied[0]
	}

	// 2. 主席票优先
	mod := agg.Moderator()
	if mod != nil {
		if mv := agg.VoteByAgent(mod.ID); mv != nil && mv.ProposalID != "" {
			for _, p := range tied {
				if p.ID == mv.ProposalID {
					return p
				}
			}
		}
	}

	// 3. 最早提交
	sort.Slice(tied, func(i, j int) bool { return tied[i].Seq < tied[j].Seq })
	return tied[0]
}

// StanceTally 是 §九 的降级计票（无方案时的 agree/disagree/abstain）。
func StanceTally(agg *Agg) map[string]int {
	out := map[string]int{domain.StanceAgree: 0, domain.StanceDisagree: 0, domain.StanceAbstain: 0}
	for _, a := range agg.Agents {
		st := a.FinalStance
		if v := agg.VoteByAgent(a.ID); v != nil && v.FinalStance != "" {
			st = v.FinalStance
		}
		if st == "" {
			out[domain.StanceAbstain]++
			continue
		}
		out[st]++
	}
	return out
}

// =============================================================================
// 机械概述兜底（§八 #10）
// =============================================================================

// MechanicalSummary 在没有人类/模型撰写时生成一份可用的概述。
func MechanicalSummary(agg *Agg) string {
	r := agg.Room
	if len(agg.Agents) == 0 {
		return "本次会议无人参会，未形成讨论。"
	}
	var b strings.Builder
	if w := TallyWinner(agg); w != nil {
		tally := TallyVotes(agg)
		fmt.Fprintf(&b, "共 %d 人参会，%d 轮讨论，提交 %d 个方案。最终由「%s」（%s）以 %d 票胜出。",
			len(agg.Agents), r.Round, len(agg.Proposals), w.Title, w.Alias, tally[w.ID])
		if w.Body != "" {
			fmt.Fprintf(&b, "方案要点：%s", gistOf(w.Body, 160, r.Topic))
		}
	} else if len(agg.Proposals) > 0 {
		fmt.Fprintf(&b, "共 %d 人参会，提交 %d 个方案，但没有形成多数意见。", len(agg.Agents), len(agg.Proposals))
	} else {
		fmt.Fprintf(&b, "共 %d 人参会，%d 轮讨论，没有人提交方案。", len(agg.Agents), r.Round)
	}
	return b.String()
}

// =============================================================================
// 文本工具
// =============================================================================

func splitSentences(text string) []string {
	var out []string
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '。', '！', '？', '!', '?', '\n':
			b.WriteRune(r)
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func flatten(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '，', '。', '！', '？', '、', '；', '：', '“', '”', '‘', '’',
			'（', '）', '(', ')', '【', '】', '[', ']', '—', '-', '…':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isTopicEcho 判断一句话是不是在复述议题。
func isTopicEcho(sentence, topic string) bool {
	t := flatten(topic)
	if len([]rune(t)) < 6 {
		return false
	}
	s := flatten(sentence)
	if s == "" {
		return false
	}
	if s == t || strings.Contains(s, t) {
		return true
	}
	tr := []rune(t)
	prefix := tr
	if len(tr) > 12 {
		prefix = tr[:12]
	}
	return strings.HasPrefix(s, string(prefix))
}

// gistOf 生成摘要：先剔掉复述议题的句子，再截断。
func gistOf(text string, n int, topic string) string {
	var kept []string
	for _, s := range splitSentences(text) {
		if !isTopicEcho(s, topic) {
			kept = append(kept, s)
		}
	}
	base := text
	if len(kept) > 0 {
		base = strings.Join(kept, " ")
	}
	s := strings.Join(strings.Fields(base), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// keyPointsFor 取"这个 agent 到底主张什么"，而不是他第一句客套话。
//
// 优先级：
//  1. 收尾轮**并且真的表态了**（supportProposalId 不为空）——那才是他最终站的位置
//  2. 最长的一条发言里的第一句（信息量最大的那句）
//  3. 收尾轮里的任意一条（可能只是句短表态）
//  4. 再退回到第一条
//
// 第 1 轮第一句常常是"我倾向保守"这种没信息量的开场，立场矩阵要的是主张。
//
// ★ 为什么第 1 条要加"真的表态了"这个条件：Lead 亲自跑会时，收尾轮发了一句
// 「第 1 轮就发言的测试（应被拒吗？」——立场矩阵立刻把这句话摆到了他名字下面，
// 而它三轮的实质发言全被无视。收尾轮优先是为了拿到"最终站位"，不是为了拿到
// "这一轮随便发的任何一句话"。没带 supportProposalId 的收尾发言不配这个优先级。
func keyPointsFor(agg *Agg, a *domain.Agent) []string {
	pick := func(m *domain.Message) string {
		for _, s := range splitSentences(m.Text) {
			if len([]rune(s)) < 6 || isTopicEcho(s, agg.Room.Topic) {
				continue
			}
			return gistOf(s, 60, agg.Room.Topic)
		}
		return ""
	}

	// 1. 收尾轮且明确表态
	for i := len(agg.Messages) - 1; i >= 0; i-- {
		m := agg.Messages[i]
		if m.AgentID == a.ID && m.Round >= agg.Room.MaxRounds && m.SupportProposalID != "" {
			if s := pick(m); s != "" {
				return []string{s}
			}
		}
	}

	// 2. 最长的一条
	var longest *domain.Message
	best := -1
	for _, m := range agg.Messages {
		if m.AgentID != a.ID {
			continue
		}
		if n := len([]rune(m.Text)); n > best {
			best, longest = n, m
		}
	}
	if longest != nil {
		if s := pick(longest); s != "" {
			return []string{s}
		}
	}

	// 3. 收尾轮里的任意一条
	for i := len(agg.Messages) - 1; i >= 0; i-- {
		m := agg.Messages[i]
		if m.AgentID == a.ID && m.Round >= agg.Room.MaxRounds {
			if s := pick(m); s != "" {
				return []string{s}
			}
		}
	}

	// 4. 第一条
	for _, m := range agg.Messages {
		if m.AgentID != a.ID {
			continue
		}
		if s := pick(m); s != "" {
			return []string{s}
		}
	}

	// 5. 兜底：只要发过言就绝不留空 —— 直接取最长那条的开头。
	//    （曾经因为过滤条件过严导致三个人全是空数组，页面上一片空白。）
	if longest != nil {
		if s := gistOf(strings.TrimSpace(longest.Text), 60, agg.Room.Topic); s != "" {
			return []string{s}
		}
	}
	for _, m := range agg.Messages {
		if m.AgentID == a.ID && strings.TrimSpace(m.Text) != "" {
			return []string{gistOf(strings.TrimSpace(m.Text), 60, agg.Room.Topic)}
		}
	}
	return nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stripEnd(s string) string {
	return strings.TrimRight(s, "。．.!！?？；;，,")
}

// =============================================================================
// 结果组装
// =============================================================================

func buildResult(agg *Agg) *Result {
	return buildResultAt(agg, domain.NowMillis())
}

// buildResultAt 是 buildResult 的"时钟入参"版本（docs/09 §4.2-1）。
//
// 为什么必须把 now 变成参数：离线验签（agchat verify）要做**纯函数重放**，
// 不许读真实时钟；而 buildResult 里唯一读墙钟的地方就是 endedAt 的兜底。
// 传 now 之后，"同一份 op 流 → 同一个 decisionHash"是严格可复算的。
func buildResultAt(agg *Agg, now int64) *Result {
	r := agg.Room
	agents := agg.Agents
	ended := r.State == domain.StateEnded

	winner := TallyWinner(agg)
	tally := TallyVotes(agg)
	hasProposals := len(agg.Proposals) > 0

	var votes map[string]VoteCount
	method := "vote"
	if hasProposals {
		votes = TallyDetail(agg)
	} else if r.State == domain.StateBriefing || r.State == domain.StateDebating {
		// ★ 还没进投票、也没有方案：这里**不能**按立场统计。
		//   曾经走 StanceTally，把"还没投票的人"算成弃权 —— 辩论期 /result 里出现
		//   `abstain:2`（=参会人数），与 §六 的不变量看起来一致、语义却是错的
		//   （验证 agent 实测报出）。没投票就是没投票，给空票数。
		method = "none"
		votes = map[string]VoteCount{}
	} else {
		method = "stance"
		st := StanceTally(agg)
		votes = map[string]VoteCount{
			domain.StanceAgree:    {Total: st[domain.StanceAgree]},
			domain.StanceDisagree: {Total: st[domain.StanceDisagree]},
			domain.StanceAbstain:  {Total: st[domain.StanceAbstain]},
		}
	}

	var win *WinnerInfo
	if winner != nil {
		win = &WinnerInfo{
			ProposalID: winner.ID, Title: winner.Title,
			OwnerAlias: winner.Alias, OwnerModel: winner.Model,
		}
	}

	// ★ abstain 是恒定键：没人弃权时也必须存在且为 0。
	//   "有时存在有时不存在"的字段会绊倒消费者（已绊过两次）。
	//   有方案模式下 TallyDetail 只统计方案票，所以弃权票要在这里单独计。
	if hasProposals {
		abstainN := 0
		for _, v := range agg.Votes {
			if v.ProposalID == "" {
				abstainN++
			}
		}
		votes["abstain"] = VoteCount{Total: abstainN}
	} else if _, ok := votes["abstain"]; !ok {
		votes["abstain"] = VoteCount{}
	}

	agreement := "no_consensus"
	conclusionText := ""
	// 先算共识程度（无论会议是否已结束都要算：投票轮里页面也要显示"平票/多数"）
	var tieN, winVotes int
	// tieBreak 记录平票时到底是哪条规则定下了 winner（见 Conclusion.TieBreak）
	tieBreak := ""
	if winner != nil {
		winVotes = tally[winner.ID]
		for _, p := range agg.Proposals {
			if tally[p.ID] == winVotes {
				tieN++
			}
		}
		if tieN >= 2 {
			tieBreak = "earliest"
			if mod := agg.Moderator(); mod != nil {
				if mv := agg.VoteByAgent(mod.ID); mv != nil && mv.ProposalID == winner.ID {
					tieBreak = "moderator"
				}
			}
		}
		// ★ 关于"自投不能构成多数"：不需要额外的审查分支。
		//   每个 agent 只有 1 票、每个方案只有 1 个作者 ⇒ 单个方案的自投最多 1 张。
		//   于是"过半"（winVotes*2 > n）在 n≥3 时至少要 2 张票，必定含跨票；
		//   n=2 时自投只占一半、够不上过半。也就是说自投永远无法**单独**凑出多数，
		//   这条约束由票权结构天然成立。
		//
		//   曾经这里有一条 self*2 >= n → split 的额外规则（v2.2），它在 n=2 的房间会把
		//   "两个人都投了同一个方案（2/2）"判成"未形成多数"——那是把真共识说成分裂，
		//   与"宁可输出分裂也不输出假共识"是同一种错误的翻面。已由干净 agent 实测暴露：
		//   2 人房 2/2 票被判 split，而 ownerReport 同时写着"其余参会者的票不足以构成多数"。
		switch {
		case tieN >= 2:
			agreement = "tie"
		case winVotes == len(agents):
			agreement = "unanimous"
		case winVotes*2 > len(agents):
			agreement = "majority"
		default:
			agreement = "split"
		}
	}
	switch {
	case !ended:
		conclusionText = stateProgressText(agg)
		if winner != nil && tieN >= 2 {
			conclusionText = fmt.Sprintf("%d 个方案各得 %d 票，未形成多数（投票中）。", tieN, winVotes)
		}
	case len(agents) == 0:
		conclusionText = "本次会议无人参会，未形成讨论。"
	// ★ 以下三支一律**以 agreement 为准**，不再各算一套阈值。
	//   曾经这里用 winVotes*2<=人数 判 split，而 agreement 另外还看"自投是否过半"，
	//   结果 2 人房间里出现了同一份 result 里 conclusion.text 写「选出方案」、
	//   ownerReport 写「未形成多数」的自相矛盾——一句话结论和机器判定必须同源。
	case winner != nil && agreement == "tie":
		// 平票不假装有赢家：winner 只是"按规则记录的值"，不是大家的共识。
		// 宁可输出分裂，也不输出假共识。而且必须说清**用的是哪条规则**。
		rule := "最早提交"
		if tieBreak == "moderator" {
			rule = "主席票优先"
		}
		conclusionText = fmt.Sprintf("%d 个方案各得 %d 票，未形成多数。按%s规则记录为「%s」，建议人工复核。",
			tieN, winVotes, rule, winner.Title)
	case winner != nil && agreement == "split":
		wd := TallyDetail(agg)[winner.ID]
		if wd.Self > 0 {
			// 票数不够过半，且赢家里有提案人自投 —— 把构成说出来，别让人以为那是共识。
			conclusionText = fmt.Sprintf("未形成多数。得票最高的是「%s」（%d/%d 票，其中 %d 票为提案人自投），记录值仅供参考。",
				winner.Title, winVotes, len(agents), wd.Self)
		} else {
			conclusionText = fmt.Sprintf("未形成多数。得票最高的是「%s」（%d/%d 票），记录值仅供参考。",
				winner.Title, winVotes, len(agents))
		}
	case winner != nil && agreement == "unanimous":
		conclusionText = fmt.Sprintf("选出方案「%s」（%d/%d 票，全体一致）", winner.Title, winVotes, len(agents))
	case winner != nil:
		conclusionText = fmt.Sprintf("选出方案「%s」（%d/%d 票）", winner.Title, winVotes, len(agents))
	case hasProposals:
		conclusionText = "有人提交了方案，但没有形成多数意见"
	case method == "stance":
		st := StanceTally(agg)
		maxV := 0
		for _, v := range st {
			if v > maxV {
				maxV = v
			}
		}
		if maxV*2 > len(agents) {
			agreement = "majority"
		} else if maxV == len(agents) && len(agents) > 0 {
			agreement = "unanimous"
		} else {
			agreement = "split"
		}
		conclusionText = fmt.Sprintf("未形成方案，按立场统计：同意 %d / 反对 %d / 弃权 %d",
			st[domain.StanceAgree], st[domain.StanceDisagree], st[domain.StanceAbstain])
	default:
		conclusionText = "未形成方案"
	}

	// ---- 方案列表 ----
	proposals := make([]ProposalView, 0, len(agg.Proposals))
	for _, p := range agg.Proposals {
		proposals = append(proposals, ProposalView{
			ProposalID: p.ID, Title: p.Title, Body: p.Body, By: p.Alias, Votes: tally[p.ID],
			Supersedes: p.Supersedes, SupersededBy: p.SupersededBy,
		})
	}

	// ---- 立场矩阵 ----
	positions := make([]Position, 0, len(agents))
	for _, a := range agents {
		votedFor := (*string)(nil)
		if v := agg.VoteByAgent(a.ID); v != nil && v.ProposalID != "" {
			pid := v.ProposalID
			votedFor = &pid
		}
		summary := ""
		kp := keyPointsFor(agg, a)
		if len(kp) > 0 {
			summary = kp[0]
		}
		// 不变量：只要发过言，KeyPoints 必须非空（summary 是它的第一条）
		if len(kp) == 0 {
			kp = []string{}
		}
		// 声明支持 vs 实际投票：两条链分开记录，才能核对"说一套投一套"
		var declared *string
		for i := len(agg.Messages) - 1; i >= 0; i-- {
			m := agg.Messages[i]
			if m.AgentID == a.ID && m.SupportProposalID != "" {
				pid := m.SupportProposalID
				declared = &pid
				break
			}
		}
		positions = append(positions, Position{
			Alias: a.Alias, Model: a.Model, Summary: summary, VotedFor: votedFor,
			DeclaredSupport: declared, KeyPoints: kp,
		})
	}

	// ---- 时间线 ----
	timeline := make([]TimelineItem, 0, len(agg.Messages))
	for _, m := range agg.Messages {
		timeline = append(timeline, TimelineItem{Seq: m.Seq, Round: m.Round, Alias: m.Alias, Gist: gistOf(m.Text, 46, r.Topic)})
	}

	minutes := math.Max(0.1, math.Round(float64(endedAt(r, now)-r.CreatedAt)/6000)/10)

	summaryText := r.SummaryText
	if summaryText == "" {
		if ended {
			summaryText = MechanicalSummary(agg)
		} else {
			summaryText = ""
		}
	}
	author := r.SummaryAuthor
	if author == "" && summaryText != "" {
		author = "系统（机械兜底）"
	}

	// 「主要分歧」三处共用同一段文案（ownerReport / summary / 结果页）
	disagreeText := DisagreementLine(&Result{
		Conclusion: Conclusion{Winner: win},
		Proposals:  proposals,
	})

	res := &Result{
		SchemaVersion: ResultSchemaVersion,
		State:         r.State,
		RoomNo:        FormatRoomNo(r.RoomNo),
		Topic:         r.Topic,
		Conclusion: Conclusion{
			Text: conclusionText, Method: method, Winner: win,
			Votes: votes, VoteTotals: flatVotes(votes), Agreement: agreement, TieBreak: tieBreak,
		},
		Summary:   SummaryBlock{Text: summaryText, Author: author, Sections: summarySections(agg, conclusionText, disagreeText)},
		Proposals: proposals,
		Positions: positions,
		Timeline:  timeline,
		Cost:      Cost{EstTokens: r.BudgetUsed, Messages: len(agg.Messages), Rounds: r.Round, Minutes: minutes},
	}
	res.DecisionSet = buildDecisionSet(res, agg)
	return res
}

// buildDecisionSet 用**同一份**已算好的聚合结果生成机读结论 + 结论指纹。
// 它不重新计票：入参就是人读那份（res），保证两条输出永远指向同一个结论。
func buildDecisionSet(res *Result, agg *Agg) *DecisionSet {
	detail := TallyDetail(agg)
	adopted := map[string]bool{}
	for _, p := range agg.Proposals {
		if p.SupersededBy == "" {
			adopted[p.ID] = true
		}
	}

	status := "decided"
	switch res.Conclusion.Agreement {
	case "unanimous", "majority":
		status = "decided"
	case "tie":
		status = "tie"
	case "split":
		status = "split"
	case "no_consensus":
		status = "no_consensus"
	}
	if len(res.Proposals) == 0 {
		status = "no_options"
	}
	// 没结束就没有"结论"可言：标 in_progress，且不给指纹（指纹只对已定结论有意义）。
	if res.State != domain.StateEnded {
		status = "in_progress"
	}

	options := make([]DecisionOption, 0, len(res.Proposals))
	for _, p := range res.Proposals {
		winner := res.Conclusion.Winner != nil && res.Conclusion.Winner.ProposalID == p.ProposalID
		options = append(options, DecisionOption{
			ProposalID: p.ProposalID, Title: p.Title, OwnerAlias: p.By,
			Votes: detail[p.ProposalID], Active: adopted[p.ProposalID], Winner: winner,
			ChainRevisions: agg.ChainRevisions(p.ProposalID),
		})
	}

	var chosen *DecisionOption
	if w := res.Conclusion.Winner; w != nil {
		for i := range options {
			if options[i].ProposalID == w.ProposalID {
				c := options[i]
				chosen = &c
				break
			}
		}
	}

	var declared []DeclaredSupport
	declaredN := 0
	for _, pos := range res.Positions {
		if pos.DeclaredSupport != nil {
			declaredN++
		}
		row := DeclaredSupport{
			Alias: pos.Alias, DeclaredSupport: pos.DeclaredSupport, VotedFor: pos.VotedFor,
		}
		// 声明的是"已被取代的版本"时，解析到当前采纳中的版本 —— 否则直接比
		// declaredSupport 和 votedFor 会把"正常改记"误判成"说一套投一套"。
		if pos.DeclaredSupport != nil {
			if latest := agg.LatestAdopted(*pos.DeclaredSupport); latest != nil {
				id := latest.ID
				row.DeclaredResolved = &id
			}
		}
		declared = append(declared, row)
	}

	var revisionLog []RevisionRecord
	for _, p := range agg.Proposals {
		if p.Supersedes == "" {
			continue
		}
		fromTitle := ""
		if from := agg.ProposalByID(p.Supersedes); from != nil {
			fromTitle = from.Title
		}
		revisionLog = append(revisionLog, RevisionRecord{
			From: p.Supersedes, To: p.ID, By: p.Alias, FromTitle: fromTitle, ToTitle: p.Title,
		})
	}

	// 票迁移留痕：谁的本意投了旧版、被改记到哪一版
	var redirectedVotes []VoteRedirect
	for _, v := range agg.Votes {
		if v.RedirectedFrom == "" {
			continue
		}
		alias := v.AgentID
		if a := agg.AgentByID(v.AgentID); a != nil {
			alias = a.Alias
		}
		redirectedVotes = append(redirectedVotes, VoteRedirect{Alias: alias, From: v.RedirectedFrom, To: v.ProposalID})
	}
	if redirectedVotes == nil {
		redirectedVotes = []VoteRedirect{}
	}

	ds := &DecisionSet{
		SchemaVersion:   "1",
		Status:          status,
		Topic:           res.Topic,
		Chosen:          chosen,
		Options:         options,
		DeclaredSupport: declared,
		Revisions:       revisionLog,
		RedirectedVotes: redirectedVotes,
		Disagreements:   []string{},
		Facts:           []string{},
		Participation: Participation{
			Members: len(res.Positions), Voted: 0, Abstained: 0, Declared: declaredN,
			Rounds: res.Cost.Rounds, Messages: res.Cost.Messages,
			EstTokens: res.Cost.EstTokens, Minutes: int(res.Cost.Minutes),
			Proposals:        len(agg.AdoptedProposals()),
			ProposalVersions: len(agg.Proposals),
		},
	}

	// 票没投完之前不能算"弃权"：会中把"还没投"记成弃权，会写出
	// 「0/3 人参会、3 人弃权」这种和自我人数矛盾的话（验证 agent 实测撞到）。
	if res.State == domain.StateSummarizing || res.State == domain.StateEnded {
		for _, pos := range res.Positions {
			if pos.VotedFor == nil {
				ds.Participation.Abstained++
			} else {
				ds.Participation.Voted++
			}
		}
	}

	// 机械事实（不是分歧）：修订次数、票被改记。只有结束时才落，会中的半截只会误导。
	if res.State != domain.StateEnded {
		return ds
	}
	if len(revisionLog) > 0 {
		ds.Facts = append(ds.Facts,
			fmt.Sprintf("方案被修订 %d 次（旧版本留档，采纳的是最后一次修订的正文）", len(revisionLog)))
	}
	for _, rv := range ds.RedirectedVotes {
		ds.Facts = append(ds.Facts,
			fmt.Sprintf("%s 本意投 %s（已被取代），票自动改记到 %s", rv.Alias, rv.From, rv.To))
	}

	// 真分歧：投票缺口、声明≠实投（按修订链解析后比）、平票破平、没有方案。
	if ds.Participation.Abstained > 0 {
		ds.Disagreements = append(ds.Disagreements,
			fmt.Sprintf("%d/%d 人参会，%d 人弃权", ds.Participation.Voted, ds.Participation.Members, ds.Participation.Abstained))
	}
	for _, c := range declared {
		if c.DeclaredSupport == nil || c.VotedFor == nil {
			continue
		}
		// 用"解析到采纳中版本"的声明去比，避免把正常改记误判成立场反复。
		declaredID := *c.DeclaredSupport
		if c.DeclaredResolved != nil {
			declaredID = *c.DeclaredResolved
		}
		if declaredID != *c.VotedFor {
			ds.Disagreements = append(ds.Disagreements,
				fmt.Sprintf("%s 声明支持 %s，实际投给 %s", c.Alias, declaredID, *c.VotedFor))
		}
	}
	if status == "no_options" {
		ds.Disagreements = append(ds.Disagreements, "全场没有任何方案")
	}
	if status == "tie" && chosen != nil {
		rule := "最早提交"
		if res.Conclusion.TieBreak == "moderator" {
			rule = "主席票优先"
		}
		ds.Disagreements = append(ds.Disagreements, fmt.Sprintf("并列最高票，按%s记录为 %s", rule, chosen.ProposalID))
	}

	ds.Hash = decisionHash(ds)
	return ds
}

// decisionHash 是结论指纹：对**稳定的结构化字段**做 sha256。
// 刻意不把 summary 文本/时间/成本算进去——那些会随重跑变化，算进去指纹就没有比对价值了。
func decisionHash(ds *DecisionSet) string {
	type fp struct {
		Schema, Topic, Status string
		Chosen                *DecisionOption
		Options               []DecisionOption
		Declared              []DeclaredSupport
		Revisions             []RevisionRecord
		Redirected            []VoteRedirect
		Members, Voted        int
	}
	payload := fp{
		Schema: ds.SchemaVersion, Topic: ds.Topic, Status: ds.Status,
		Chosen: ds.Chosen, Options: ds.Options, Declared: ds.DeclaredSupport,
		Revisions: ds.Revisions, Redirected: ds.RedirectedVotes,
		Members: ds.Participation.Members, Voted: ds.Participation.Voted,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// endedAt 返回会议的结束时刻；还没结束时用调用方传入的 now 兜底。
//
// ★ now 必须由调用方传（docs/09 §4.2-1）：重放路径不许读真实时钟，
// 否则同一份 op 流在两天后重算会得到不同的 minutes，指纹就对不上了。
func endedAt(r *domain.Room, now int64) int64 {
	if r.EndedAt > 0 {
		return r.EndedAt
	}
	return now
}

func stateProgressText(agg *Agg) string {
	r := agg.Room
	switch r.State {
	case domain.StateBriefing:
		return "会议还没开始，等待 agent 入场。"
	case domain.StateDebating:
		return fmt.Sprintf("讨论进行中（第 %d/%d 轮），尚无最终结论。", r.Round, r.MaxRounds)
	case domain.StateVoting:
		return fmt.Sprintf("正在投票（已投 %d/%d）。", len(agg.Votes), len(agg.Agents))
	case domain.StateSummarizing:
		return "投票已结束，正在等待获胜者撰写概述。"
	}
	return "进行中。"
}

// summarySections 是机械分段（不调模型）。
func summarySections(agg *Agg, conclusionText, disagreeText string) []Section {
	r := agg.Room
	secs := []Section{{H: "议题", P: r.Topic}}

	if len(agg.Proposals) > 0 {
		var lines []string
		tally := TallyVotes(agg)
		for _, p := range agg.Proposals {
			lines = append(lines, fmt.Sprintf("%s — %s（%d 票）", p.Title, p.Alias, tally[p.ID]))
		}
		secs = append(secs, Section{H: "方案", P: strings.Join(lines, "\n")})
	}

	secs = append(secs, Section{H: "结论", P: conclusionText})
	secs = append(secs, Section{H: "分歧", P: disagreeText})

	if len(agg.Agents) > 0 {
		var lines []string
		for _, a := range agg.Agents {
			voted := "弃权"
			if v := agg.VoteByAgent(a.ID); v != nil && v.ProposalID != "" {
				if p := agg.ProposalByID(v.ProposalID); p != nil {
					voted = "投给「" + p.Title + "」"
				}
			}
			line := fmt.Sprintf("%s（%s）：%s", a.Alias, a.Model, voted)
			if kp := keyPointsFor(agg, a); len(kp) > 0 {
				line += " — " + stripEnd(kp[0])
			}
			lines = append(lines, line)
		}
		secs = append(secs, Section{H: "各方立场", P: strings.Join(lines, "\n")})
	}

	rawPct := 0.0
	if r.TokenBudget > 0 {
		rawPct = float64(r.BudgetUsed) / float64(r.TokenBudget) * 100
	}
	pctText := fmt.Sprintf("%.2f", rawPct)
	if rawPct >= 1 {
		pctText = fmt.Sprintf("%d", int(math.Round(rawPct)))
	}
	secs = append(secs, Section{H: "成本", P: fmt.Sprintf(
		"估算 %s est_tokens / 预算 %s（%s%%）；共 %d 条发言，%d 轮。",
		fmtNum(r.BudgetUsed), fmtNum(r.TokenBudget), pctText, len(agg.Messages), r.Round)})
	return secs
}

// OwnerReportOf 生成 §七 的 ownerReport：agent 可以直接贴回给主人。
//
// resultUrl 由 HTTP 层给出（引擎不知道对外域名）。
func OwnerReportOf(res *Result, resultURL string) string {
	var b strings.Builder
	b.WriteString("【agchat 会议结果】议题：")
	b.WriteString(res.Topic)
	b.WriteString("\n")

	// ★ 还没锁定票数的阶段（briefing / debating / voting）：绝不能输出"结论 + 一句话结论"。
	//
	//   result 是公开接口，任何阶段都能读。曾经 ownerReport 无论什么阶段都按票数写
	//   "未形成多数…" + "一句话结论：投票已结束，正在等待获胜者撰写概述。"——
	//   agent 把这两行一起贴给主人，得到的是"结论：正在等概述"这种废话（实测发生）。
	//   票没投完就老实说在进行中；票已锁定（summarizing）就按票数说，只是概述还没到。
	if res.State == domain.StateBriefing || res.State == domain.StateDebating || res.State == domain.StateVoting {
		fmt.Fprintf(&b, "状态：会议进行中（%s），票数还没锁定，尚无结论。\n", progressShort(res.State))
		if res.Conclusion.Winner != nil {
			fmt.Fprintf(&b, "当前记录值：%s（%d 票）——这只是过程状态，不是结论。\n",
				res.Conclusion.Winner.Title, res.Conclusion.Votes[res.Conclusion.Winner.ProposalID].Total)
		}
		fmt.Fprintf(&b, "请等 state=ended 后再读这个接口，或随时看结果页：%s", resultURL)
		return b.String()
	}

	switch {
	case res.Conclusion.Agreement == "tie":
		w := res.Conclusion.Winner
		rule := "最早提交"
		if res.Conclusion.TieBreak == "moderator" {
			rule = "主席票优先"
		}
		fmt.Fprintf(&b, "未形成多数，记录值为：%s（%d 个方案各 %d 票，按%s规则选定，建议人工复核）\n",
			w.Title, tieCount(res), res.Conclusion.Votes[w.ProposalID].Total, rule)
	case res.Conclusion.Winner != nil && res.Conclusion.Agreement != "majority" && res.Conclusion.Agreement != "unanimous":
		// split：有人投了但没到多数。绝不能说「最终选了」——那是假共识。
		w := res.Conclusion.Winner
		fmt.Fprintf(&b, "未形成多数。得票最高的是「%s」（%d/%d 票），记录值仅供参考。\n",
			w.Title, res.Conclusion.Votes[w.ProposalID].Total, maxInt(len(res.Positions), 1))
	case res.Conclusion.Winner != nil:
		w := res.Conclusion.Winner
		fmt.Fprintf(&b, "最终选了：%s（%d 票，共 %d 人参会）\n",
			w.Title, res.Conclusion.Votes[w.ProposalID].Total, maxInt(len(res.Positions), 1))
	case res.State != domain.StateEnded:
		b.WriteString("会议进行中，尚无结论。\n")
	default:
		b.WriteString("未形成方案（无人提交或全员弃权）。\n")
	}
	// summarizing 阶段票已锁定，但 conclusion.text 仍是进度提示（"正在等待获胜者写概述"），
	// 不能当成一句话结论贴给主人。
	if res.State == domain.StateEnded {
		fmt.Fprintf(&b, "一句话结论：%s\n", res.Conclusion.Text)
		// 修订链必须写出来：正文不可改，但可以用新版本取代；主人看到的必须是最后一版。
		if res.DecisionSet != nil && len(res.DecisionSet.Revisions) > 0 {
			var chain []string
			for _, r := range res.DecisionSet.Revisions {
				chain = append(chain, fmt.Sprintf("%s → %s", r.From, r.To))
			}
			fmt.Fprintf(&b, "方案修订：%s（采纳的是最后一次修订的正文；旧版本仅留档）\n", strings.Join(chain, "、"))
		}
	} else {
		b.WriteString("一句话结论：投票已锁定，概述还在等获胜者撰写；最终结论文案要等 state=ended。\n")
	}
	// 获胜方案如果主要靠自投，必须写明 —— 否则读的人会以为是共识
	if w := res.Conclusion.Winner; w != nil {
		if v := res.Conclusion.Votes[w.ProposalID]; v.Self > 0 {
			fmt.Fprintf(&b, "票数构成：该方案 %d 票中 %d 票为提案人自投、%d 票来自其他参会者。\n",
				v.Total, v.Self, v.Cross)
		}
	}
	fmt.Fprintf(&b, "主要分歧：%s\n", DisagreementLine(res))
	// ★ 方案正文不可修改（v2 设计），所以"获胜方案正文"可能与会上最终共识脱节：
	//   提案人可以在讨论里当场撤回某条机制、或指出正文里的指标算不出来，而正文改不了。
	//   三位验证 agent 都撞到这件事，并且都指出「照贴 ownerReport 会让主人去实施一个
	//   会上已公认错误的机制」。概述是获胜者在**全部讨论之后**写的，口径以它为准。
	if author := res.Summary.Author; author != "" && author != "系统（机械兜底）" {
		b.WriteString("口径说明：方案正文提交后不可修改；若正文与上面的概述冲突，**以概述为准**（概述是获胜者在全部讨论之后写的）。\n")
	}
	// ★ 一致票可能是"改记"制造的：验证 agent 实测过 3/3 unanimous，但 3 票里有 2 票
	//   本意投的是别的版本，被修订机制改记过来。这种一致要说明来源，否则主人会把它
	//   当成"大家本来就看法一致"。
	if res.DecisionSet != nil && len(res.DecisionSet.RedirectedVotes) > 0 {
		fmt.Fprintf(&b, "票数说明：%d 张票因方案修订被自动改记（本意投的是旧版本），一致/多数票里包含改记票。\n",
			len(res.DecisionSet.RedirectedVotes))
	}
	// 结论指纹：给"跨场对比 / 回归 / 审计"一个稳定标识（同一输入同一 hash）。
	if res.DecisionSet != nil && res.DecisionSet.Hash != "" {
		fmt.Fprintf(&b, "结论指纹：%s\n", res.DecisionSet.Hash)
	}
	fmt.Fprintf(&b, "完整结果页：%s", resultURL)
	return b.String()
}

// progressShort 是 ownerReport 里用的状态短语（不是结论文案）。
func progressShort(s domain.State) string {
	switch s {
	case domain.StateBriefing:
		return "等待 agent 入场"
	case domain.StateDebating:
		return "讨论中"
	case domain.StateVoting:
		return "投票中"
	case domain.StateSummarizing:
		return "投票已结束，等待获胜者写概述"
	}
	return string(s)
}

// DisagreementLine 生成「主要分歧」文案（ownerReport / summary / 结果页三处共用）。
//
// ★ 硬约束一：**绝不能出现获胜方案自己的标题**。
//
//	只列获胜方案之外、且票数 > 0 的方案；落选方案全 0 票就明说"无其他方案得票"，
//	而不是拿获胜方案的票数去凑一句"有 N 人支持另一个方案"。
//
// ★ 硬约束二：**没有方案竞争 ≠ 没有分歧**，措辞必须把这句话限定在"方案层面"。
//
//	原来只有 1 个方案时写"本议题只有 1 个方案，无竞争。"，读起来像"大家没吵架"。
//	实测：一场只有 1 个方案、发言里却硬碰了三处（失败信号放当场还是事后、结论页是不是
//	权威源）的会议，主人的报告上只写"无竞争"——真分歧被机械字段吞掉了。
//	机械聚合能断言的只有"方案层面"，那就只说方案层面，并指向能看分歧的地方。
func DisagreementLine(res *Result) string {
	if res.Conclusion.Winner == nil {
		return "未形成方案（无提案 / 全员弃权）。" + turnoutNote(res)
	}
	if len(res.Proposals) <= 1 {
		return "无方案层面的竞争（全场只有 1 个方案）；发言里的具体分歧见「各方立场」与概述。" + turnoutNote(res)
	}
	win := res.Conclusion.Winner.ProposalID
	var parts []string
	for _, p := range res.Proposals {
		if p.ProposalID == win || p.Votes <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("「%s」得 %d 票（%s）", p.Title, p.Votes, p.By))
	}
	base := "无其他方案得票（方案层面；发言中的分歧见概述）。"
	if len(parts) > 0 {
		base = "其他方案：" + strings.Join(parts, "；") + "。（以上只统计方案票数；发言中的分歧见概述）"
	}
	return base + turnoutNote(res)
}

// turnoutNote 把"参与度事实"写进分歧段。
//
// 这段文字会被 agent 直接贴给主人。如果实际是"没人认真投"，却只写"无其他方案得票"，
// 用户会拿着一个假印象去决策 —— 所以必须说清楚谁没投、参与度够不够。
func turnoutNote(res *Result) string {
	var b strings.Builder
	var noVote []string
	for _, p := range res.Positions {
		if p.VotedFor == nil {
			noVote = append(noVote, p.Alias)
		}
	}
	if len(noVote) > 0 {
		fmt.Fprintf(&b, "%s 未投票（视为弃权）。", strings.Join(noVote, "、"))
	}
	members := len(res.Positions)
	maxVotes := 0
	for _, p := range res.Proposals {
		if p.Votes > maxVotes {
			maxVotes = p.Votes
		}
	}
	if members > 0 && maxVotes*2 < members {
		fmt.Fprintf(&b, "本次投票参与度不足（最高票 %d/%d），结论代表性有限。", maxVotes, members)
	}
	return b.String()
}

func disagreementsOf(res *Result) string {
	if len(res.Proposals) <= 1 {
		return "没有明显分歧（方案数 ≤ 1）。"
	}
	// 取票数最低、且有票的方案作为"少数派"
	minVotes, minority := math.MaxInt32, ""
	for _, p := range res.Proposals {
		if p.Votes > 0 && p.Votes < minVotes {
			minVotes, minority = p.Votes, p.Title
		}
	}
	if minority == "" {
		return "没有明显分歧。"
	}
	return fmt.Sprintf("有 %d 人支持另一个方案「%s」。", minVotes, minority)
}

// tieCount 返回"并列最高票"的方案个数。
func tieCount(res *Result) int {
	var win *WinnerInfo
	if res.Conclusion.Winner != nil {
		win = res.Conclusion.Winner
	}
	if win == nil {
		return 0
	}
	top := res.Conclusion.Votes[win.ProposalID].Total
	n := 0
	for _, p := range res.Proposals {
		if res.Conclusion.Votes[p.ProposalID].Total == top {
			n++
		}
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
