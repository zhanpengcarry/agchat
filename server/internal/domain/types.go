// Package domain 定义 agchat 的核心数据模型（v2 会议流程）。
//
// 字段名与数据库列名、JSON 字段名保持一致：
//   - JSON：对外契约（见 docs/07-会议流程规格v2.md）
//   - DB：迁移脚本里的列名（一行对一列，方便将来换 Postgres）
//
// 时间一律用 int64 毫秒时间戳存储（0 表示 null）。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// State 是房间的生命周期状态。v2 用 voting / summarizing 取代了 v1 的 converging。
type State string

const (
	StateBriefing    State = "briefing"    // 等 agent 入场
	StateDebating    State = "debating"    // 立场轮 / 交锋轮 / 收尾轮
	StateVoting      State = "voting"      // 投票中
	StateSummarizing State = "summarizing" // 获胜者写概述
	StateEnded       State = "ended"       // 结果只读
)

// 发言姿态（v1 兼容：没有方案时的降级投票用它）。
const (
	StanceAgree    = "agree"
	StanceDisagree = "disagree"
	StanceAbstain  = "abstain"
)

// StanceCN 是姿态的中文标签。
var StanceCN = map[string]string{
	StanceAgree:    "同意",
	StanceDisagree: "反对",
	StanceAbstain:  "弃权",
}

// Room 是一次会议的全部可变状态。
type Room struct {
	ID       string
	TenantID string // 数据隔离的第一维

	RoomNo string // 9 位会议号（对外心智：像腾讯会议那样报数字）
	Topic  string
	Rules  string

	State State
	Round int

	MaxRounds       int
	MaxAgents       int
	MaxMinutes      int
	MaxOutputTokens int

	// 预算账本（单位 est_tokens）
	TokenBudget   int
	BudgetUsed    int
	BudgetRead    int
	BudgetWritten int
	BudgetOverrun bool

	// 主席：第一个成功 join 的 agent（并发下靠房间锁 + 事务保证恰好一个）
	ModeratorID string

	// 凭证只存哈希，明文仅在创建/加入响应里出现一次
	PasswordHash string
	AdminKeyHash string

	CreatedAt         int64
	StartedAt         int64 // 0 = 未开始
	EndedAt           int64 // 0 = 未结束
	BriefingDeadline  int64
	EndReason         string
	ConvergenceReason string // 进入 voting 的原因：rounds_done / budget / time_limit / chain_timeout / admin_close
	PhaseStartedAt    int64  // 进入当前阶段的时间（投票截止、概述超时用）

	Seq  int
	Plan *Plan

	ChainTimeouts  int   // 人次超时计数（仅诊断用；跳轮判据已改为 RoundIdleSince）
	EmptyRounds    int   // 连续空轮数（整轮零发言，连续 3 次直接进投票）
	RoundSkipped   bool  // 当前轮是否被判为空轮
	RoundIdleSince int64 // 本轮最后一次有人发言的时间；整轮无人 90s 直接跳轮

	// 结尾产物
	WinnerProposalID string
	SummaryText      string
	SummaryAuthor    string
	SummaryAt        int64

	ResultJSON string // 结束时冻结的结论
}

// Plan 是本轮发言权的串行队列。
type Plan struct {
	Round         int      `json:"round"`
	Queue         []string `json:"queue"`
	Pos           int      `json:"pos"`
	TurnStartedAt int64    `json:"turnStartedAt"`
	Skipped       []string `json:"skipped"`
}

// Agent 是加入房间的一个 AI agent。
type Agent struct {
	ID       string
	TenantID string
	RoomID   string

	Alias string
	Model string

	TokenHash string
	JoinedAt  int64
	Ord       int // 入场顺序（0 基）；主席 = Ord 0

	QuotaTokens   int
	TokensRead    int
	TokensWritten int
	TokensTotal   int

	LastSeenAt      int64 // 最近一次带 token 的请求时间（reclaim 防双开用）
	ReadCursor      int
	FinalStance     string // 降级投票（无方案时）
	LastStanceRound int
}

// IsModerator 判断是不是主席。主席的身份就是"第一个进场的人"。
func (a *Agent) IsModerator(r *Room) bool { return r.ModeratorID != "" && r.ModeratorID == a.ID }

// Proposal 是一个方案（第 2..N-1 轮提交，每人最多 2 个，提交后不可改）。
type Proposal struct {
	TenantID  string
	RoomID    string
	ID        string // p_1, p_2 …
	Seq       int    // 提交时房间的消息序号
	AgentID   string
	Alias     string
	Model     string
	Title     string
	Body      string
	CreatedAt int64
	// Supersedes / SupersededBy 构成修订链：正文不可改，但可以用新版本取代旧版本。
	// SupersededBy == "" 表示"当前采纳中"；旧版本仍在库里可追溯。
	Supersedes   string
	SupersededBy string
}

// Vote 是一票。ProposalID 为空表示弃权；降级投票时 FinalStance 有值。
type Vote struct {
	TenantID    string
	RoomID      string
	AgentID     string
	ProposalID  string // "" = 弃权
	FinalStance string // 降级投票用（无方案时）
	Reason      string
	CreatedAt   int64
	// RedirectedFrom 非空 = 这一票本来投给该方案，但它已被修订取代，票自动改记到 ProposalID。
	// 留痕目的：结论里要能看见"本意 vs 实际"（验证 agent 指出票迁移在哈希域里不可见）。
	RedirectedFrom string
}

// Message 是一条发言。
type Message struct {
	TenantID string
	RoomID   string
	Seq      int

	Round   int
	AgentID string
	Alias   string
	Model   string
	Text    string

	ReplyTo           *int
	SupportProposalID string // 收尾轮：说明支持哪个方案
	Stance            string // v1 兼容
	Mentions          []string
	TS                string
	OpKey             string
}

// Estimate 是建会时给用户的预算估算。
type Estimate struct {
	Tokens    int    `json:"tokens"`
	Low       int    `json:"low"`
	High      int    `json:"high"`
	WorstCase int    `json:"worstCase"`
	Basis     string `json:"basis"`
	Fits      bool   `json:"fits"`
}

// Error 是唯一被 HTTP 层认识的错误类型。
type Error struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError 造一个业务错误。
func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// With 附加响应体里的额外字段。
func (e *Error) With(kv map[string]any) *Error {
	e.Extra = kv
	return e
}

// ErrNotFound 是跨租户/跨房间访问的统一出口（404 不 403）。
var ErrNotFound = NewError(404, "not_found", "会议不存在或无权访问")

// NowMillis 是统一的时间来源。
func NowMillis() int64 { return time.Now().UnixMilli() }

// HashToken 是凭证落库用的哈希（sha256 hex）。
func HashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
