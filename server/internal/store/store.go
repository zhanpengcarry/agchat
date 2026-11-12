// Package store 是唯一的持久化入口，也是数据隔离的第二层（Go 层强制作用域）。
//
// ============================================================================
// 三条铁律（改动这个包之前请先读完）
// ============================================================================
//
//  1. *sql.DB 是包内私有字段。它不会、也不能被 api 层的 handler 拿到。
//     handler 想读业务表，唯一的路是 Store.WithTenant。
//
//  2. tenant_id 由 Tx 的私有字段提供，**调用方没有机会传**。
//     每个 SQL 都把 `WHERE tenant_id = ?` 写在最前面，参数从 t.tenantID 注入。
//     这不是"约定"，是类型签名决定的：Tx 的公开方法没有一个接收 tenantID。
//
//  3. 每条 SQL 在执行前都要过 assertTenantScope。
//     凡是 SELECT/UPDATE/DELETE 缺 `tenant_id = ?`、或者 INSERT 没写 tenant_id 列，
//     都会在运行期直接报错。配套的测试（store_scope_test.go）还会静态扫描
//     本包与 api 包里的所有 SQL 字面量——遗漏在测试期就会炸，而不是等到线上。
//
// 为什么值得这么麻烦：租户 ID 一旦能被调用方传入，就等于每个新接口都是一次
// 越权风险，而且代码评审很难看出来（`WHERE id = ?` 和 `WHERE tenant_id = ? AND id = ?`
// 长得太像了）。把能力收进 WithTenant，让"忘记加租户条件"变成**写不出来**的代码。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agchat/internal/domain"
)

// Store 持有数据库连接池。db 私有，外部无法绕过租户作用域。
type Store struct {
	db *sql.DB
}

// Tx 是一个**已经绑定租户**的事务。
//
// 调用方拿不到 tx.tenantID，也没有任何方法可以改它或额外传一个 tenantID。
type Tx struct {
	ctx      context.Context
	tx       *sql.Tx
	tenantID string
}

// TenantID 只读地暴露当前租户（用于日志、审计）。
func (t *Tx) TenantID() string { return t.tenantID }

// =============================================================================
// 租户作用域守卫
// =============================================================================

// assertTenantScope 校验 SQL 文本真的带了租户条件。
//
// 这是"让遗漏在测试期就炸"的那一行：它不是安全边界（安全边界是 tenant_id 由
// Tx 私有注入），而是**防止未来有人复制粘贴出一条漏条件的 SQL**。
func assertTenantScope(query string) error {
	norm := strings.ToLower(strings.Join(strings.Fields(query), " "))
	if norm == "" {
		return errors.New("store: 空 SQL")
	}
	if strings.HasPrefix(norm, "insert") {
		// INSERT 没有 WHERE，要求列清单里出现 tenant_id
		if strings.Contains(norm, "(tenant_id") || strings.Contains(norm, ", tenant_id") {
			return nil
		}
		return fmt.Errorf("store: INSERT 缺少 tenant_id 列: %s", query)
	}
	if !strings.Contains(norm, "tenant_id = ?") {
		return fmt.Errorf("store: 查询缺少租户作用域 (tenant_id = ?): %s", query)
	}
	return nil
}

// exec / query 是所有写读的唯一出口：先过守卫，再执行。
func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	if err := assertTenantScope(query); err != nil {
		return nil, err
	}
	return t.tx.ExecContext(t.ctx, query, args...)
}

func (t *Tx) query(query string, args ...any) (*sql.Rows, error) {
	if err := assertTenantScope(query); err != nil {
		return nil, err
	}
	return t.tx.QueryContext(t.ctx, query, args...)
}

// queryRow 返回单行查询。守卫失败时返回 error 而不是构造一条假 SQL ——
// 假 SQL 会污染静态扫描，也让失败原因变得不透明。
func (t *Tx) queryRow(query string, args ...any) (*sql.Row, error) {
	if err := assertTenantScope(query); err != nil {
		return nil, err
	}
	return t.tx.QueryRowContext(t.ctx, query, args...), nil
}

// =============================================================================
// 入口
// =============================================================================

// WithTenant 是访问业务表的唯一入口。
//
// 它开一个事务、把 tenantID 绑定到 Tx、执行 fn、提交。
// fn 里任何一条 SQL 都会自动带上这个 tenantID。
func (s *Store) WithTenant(ctx context.Context, tenantID string, fn func(*Tx) error) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("store: 空 tenantID —— 拒绝在无租户作用域下访问业务表")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &Tx{ctx: ctx, tx: tx, tenantID: tenantID}
	if err := fn(t); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReadOnly 与 WithTenant 相同，但开的是**只读事务**（不开写锁、不 fsync）。
//
// 为什么单独一条：快照类读取（状态、head、结果预览）在每次请求里都会发生，
// 而 WithTenant 走的是 BEGIN IMMEDIATE，一次只读也要付写锁与提交的钱。
// 只读路径最后一定 Rollback —— 它本来就没有要提交的东西。
func (s *Store) ReadOnly(ctx context.Context, tenantID string, fn func(*Tx) error) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("store: 空 tenantID —— 拒绝在无租户作用域下访问业务表")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	t := &Tx{ctx: ctx, tx: tx, tenantID: tenantID}
	if err := fn(t); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// NewTenant 建一个匿名租户，返回 tenantId。
//
// 这是**唯一**允许在租户作用域之外写入的语句：tenants 表本身就是租户，
// 它没有也不可能有 tenant_id 列。除此之外没有任何表可以被无作用域写入。
func (s *Store) NewTenant(ctx context.Context) (string, error) {
	id, err := randomID("t_", 8)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tenants (id, created_at) VALUES (?, ?)`,
		id, domain.NowMillis(),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Ping 给 /healthz 用。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close 关连接池。
func (s *Store) Close() error { return s.db.Close() }

// ResolveRoom 把 roomId 翻译成 tenantId。
//
// ★ 这是全服务**唯一**一处不带租户作用域的查询。它只能返回一个 tenantId，
// 返回不了任何业务内容。得到 tenantId 之后，所有读写都必须走 WithTenant，
// 否则 assertTenantScope 会直接报错。
//
// 没有它就绕不过去：对外契约里客户端只有 roomId，没有 tenantId，
// 服务端必须先定位租户，才能进入租户作用域。
func (s *Store) ResolveRoom(ctx context.Context, roomID string) (string, error) {
	var tenantID string
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM room_directory /* global-routing */ WHERE room_id = ?`, roomID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// PutRoomDirectory 在**同一个租户事务里**写入路由行，保证房间与路由原子出现。
// room_no（9 位会议号）也登记在这里，让会议号能路由到租户。
func (t *Tx) PutRoomDirectory(roomID, roomNo string) error {
	_, err := t.exec(`INSERT INTO room_directory (room_id, tenant_id, room_no, created_at) VALUES (?,?,?,?)`,
		roomID, t.tenantID, roomNo, domain.NowMillis())
	return err
}

// ResolveRoomKey 把"会议号"或"内部 roomId"翻译成 (tenantId, roomId)。
//
// 会议号允许带空格/连字符（728 451 936），去掉非数字后按 9 位匹配；
// 否则当成 roomId。找不到一律返回 domain.ErrNotFound，不区分"格式错"和"不存在"
// —— 否则就成了一个枚举 oracle。
//
// 与 ResolveRoom 一样，这是不带租户作用域的定位查询，只读 room_directory 这张映射表。
func (s *Store) ResolveRoomKey(ctx context.Context, key string) (tenantID, roomID string, err error) {
	no := normalizeRoomNo(key)
	var q string
	var args []any
	if no != "" {
		q = `SELECT tenant_id, room_id FROM room_directory /* global-routing */ WHERE room_no = ? OR room_id = ?`
		args = []any{no, key}
	} else {
		q = `SELECT tenant_id, room_id FROM room_directory /* global-routing */ WHERE room_id = ?`
		args = []any{key}
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&tenantID, &roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", domain.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return tenantID, roomID, nil
}

// normalizeRoomNo 提取 9 位会议号；不像会议号时返回空。
func normalizeRoomNo(key string) string {
	var b strings.Builder
	for _, r := range key {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) == 9 {
		return s
	}
	return ""
}

// PutTokenDirectory 记录 token 的归属（租户/房间/agent）。
// 与 PutRoomDirectory 一样，在 join 的同一个租户事务里写，保证原子。
func (t *Tx) PutTokenDirectory(tokenHash, roomID, agentID string) error {
	_, err := t.exec(`INSERT INTO token_directory (token_hash, tenant_id, room_id, agent_id, created_at)
		VALUES (?,?,?,?,?)`, tokenHash, t.tenantID, roomID, agentID, domain.NowMillis())
	return err
}

// TokenRef 是一枚 token 的归属信息。
type TokenRef struct {
	TenantID string
	RoomID   string
	AgentID  string
}

// LookupTokenTenant 查一枚 token 归属于哪个租户/房间。
//
// ★ 与 ResolveRoom 并列的第二个（也是最后一个）无租户作用域查询。
//
//	它只读 token_directory 这张窄表（哈希 → 归属），不碰任何业务内容；
//	而且**只在 404 失败路径上调用**，用来写审计日志。
//	明文 token 从不落库，这里拿到的也只是哈希。
func (s *Store) LookupTokenTenant(ctx context.Context, tokenHash string) (TokenRef, bool, error) {
	var ref TokenRef
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, room_id, agent_id FROM token_directory /* global-routing */ WHERE token_hash = ?`,
		tokenHash).Scan(&ref.TenantID, &ref.RoomID, &ref.AgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRef{}, false, nil
	}
	if err != nil {
		return TokenRef{}, false, err
	}
	return ref, true, nil
}

// dummyTenantID 是一个永远不存在的租户。
//
// 它存在的唯一目的：给"房间不存在"这条路径补上**与"房间存在但无权"完全相同的
// 数据库工作量**（一个事务 + 同样三条索引查询），从而抹掉存在性计时侧信道。
// 注意这不是 time.Sleep —— 那只是假装在同一台机器上"差不多快"，
// 换个机器/换个 SQLite 版本比例就飘了；而真做同样的功，比例天然是 1。
const dummyTenantID = "__dummy_tenant_for_timing_parity__"

// DummyScopedLoad 执行与 loadAgg 完全相同的三条查询，但注定查不到任何东西。
//
// 三连查全部执行、不提前返回：真实路径在房间存在时会依次读 rooms / agents / messages，
// 这里必须同样跑三次索引探测，往返次数才对得上。
func (s *Store) DummyScopedLoad(ctx context.Context, roomID string) error {
	return s.WithTenant(ctx, dummyTenantID, func(tx *Tx) error {
		// 用**真实的 roomID** 做键：同样的索引、同样的键长度，只是租户前缀注定查不到
		if _, err := tx.GetRoom(roomID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if _, err := tx.ListAgents(roomID); err != nil {
			return err
		}
		if _, err := tx.ListMessages(roomID, 0); err != nil {
			return err
		}
		return nil
	})
}

func randomID(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// =============================================================================
// rooms
// =============================================================================

const roomCols = `tenant_id, id, room_no, topic, rules, state, round, max_rounds, max_agents,
	minutes_limit, max_output_tokens, token_budget, budget_used, budget_read, budget_written,
	budget_overrun, moderator_id, password_hash, admin_key_hash, created_at, started_at, ended_at,
	briefing_deadline, end_reason, convergence_reason, phase_started_at, chain_timeouts,
	seq, round_idle_since, empty_rounds, round_skipped, plan_json, winner_proposal_id,
	summary_text, summary_author, summary_at, result_json`

// CreateRoom 插入房间。tenant_id 取自 Tx，调用方传不进来。
func (t *Tx) CreateRoom(r *domain.Room) error {
	q := `INSERT INTO rooms (tenant_id, id, room_no, topic, rules, state, round, max_rounds,
		max_agents, minutes_limit, max_output_tokens, token_budget, budget_used, budget_read,
		budget_written, budget_overrun, moderator_id, password_hash, admin_key_hash, created_at,
		started_at, ended_at, briefing_deadline, end_reason, convergence_reason, phase_started_at,
		chain_timeouts, seq, round_idle_since, empty_rounds, round_skipped, plan_json,
		winner_proposal_id, summary_text, summary_author, summary_at, result_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	planJSON, err := encodePlan(r.Plan)
	if err != nil {
		return err
	}
	_, err = t.exec(q, t.tenantID, r.ID, r.RoomNo, r.Topic, r.Rules, string(r.State), r.Round,
		r.MaxRounds, r.MaxAgents, r.MaxMinutes, r.MaxOutputTokens, r.TokenBudget, r.BudgetUsed,
		r.BudgetRead, r.BudgetWritten, boolToInt(r.BudgetOverrun), r.ModeratorID, r.PasswordHash,
		r.AdminKeyHash, r.CreatedAt, r.StartedAt, r.EndedAt, r.BriefingDeadline, r.EndReason,
		r.ConvergenceReason, r.PhaseStartedAt, r.ChainTimeouts, r.Seq, r.RoundIdleSince,
		r.EmptyRounds, boolToInt(r.RoundSkipped), planJSON,
		r.WinnerProposalID, r.SummaryText, r.SummaryAuthor, r.SummaryAt, r.ResultJSON)
	return err
}

// GetRoom 按 (tenant, id) 取房间。不属于本租户时返回 domain.ErrNotFound —— 不是 403。
func (t *Tx) GetRoom(roomID string) (*domain.Room, error) {
	row, err := t.queryRow(`SELECT `+roomCols+` FROM rooms WHERE tenant_id = ? AND id = ?`, t.tenantID, roomID)
	if err != nil {
		return nil, err
	}
	var r domain.Room
	var state, planJSON string
	var overrun, skipped int
	err = row.Scan(&r.TenantID, &r.ID, &r.RoomNo, &r.Topic, &r.Rules, &state, &r.Round,
		&r.MaxRounds, &r.MaxAgents, &r.MaxMinutes, &r.MaxOutputTokens, &r.TokenBudget,
		&r.BudgetUsed, &r.BudgetRead, &r.BudgetWritten, &overrun, &r.ModeratorID,
		&r.PasswordHash, &r.AdminKeyHash, &r.CreatedAt, &r.StartedAt, &r.EndedAt,
		&r.BriefingDeadline, &r.EndReason, &r.ConvergenceReason, &r.PhaseStartedAt,
		&r.ChainTimeouts, &r.Seq, &r.RoundIdleSince, &r.EmptyRounds, &skipped, &planJSON, &r.WinnerProposalID, &r.SummaryText,
		&r.SummaryAuthor, &r.SummaryAt, &r.ResultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.State = domain.State(state)
	r.BudgetOverrun = overrun != 0
	r.RoundSkipped = skipped != 0
	if r.Plan, err = decodePlan(planJSON); err != nil {
		return nil, err
	}
	return &r, nil
}

// SaveRoom 整行更新。WHERE 前缀同样是 tenant_id = ?。
func (t *Tx) SaveRoom(r *domain.Room) error {
	planJSON, err := encodePlan(r.Plan)
	if err != nil {
		return err
	}
	q := `UPDATE rooms SET room_no = ?, topic = ?, rules = ?, state = ?, round = ?,
		max_rounds = ?, max_agents = ?, minutes_limit = ?, max_output_tokens = ?,
		token_budget = ?, budget_used = ?, budget_read = ?, budget_written = ?,
		budget_overrun = ?, moderator_id = ?, password_hash = ?, admin_key_hash = ?,
		created_at = ?, started_at = ?, ended_at = ?, briefing_deadline = ?, end_reason = ?,
		convergence_reason = ?, phase_started_at = ?, chain_timeouts = ?, seq = ?, round_idle_since = ?,
		empty_rounds = ?, round_skipped = ?,
		plan_json = ?, winner_proposal_id = ?, summary_text = ?, summary_author = ?,
		summary_at = ?, result_json = ?
		WHERE tenant_id = ? AND id = ?`
	_, err = t.exec(q, r.RoomNo, r.Topic, r.Rules, string(r.State), r.Round, r.MaxRounds,
		r.MaxAgents, r.MaxMinutes, r.MaxOutputTokens, r.TokenBudget, r.BudgetUsed, r.BudgetRead,
		r.BudgetWritten, boolToInt(r.BudgetOverrun), r.ModeratorID, r.PasswordHash,
		r.AdminKeyHash, r.CreatedAt, r.StartedAt, r.EndedAt, r.BriefingDeadline, r.EndReason,
		r.ConvergenceReason, r.PhaseStartedAt, r.ChainTimeouts, r.Seq, r.RoundIdleSince,
		r.EmptyRounds, boolToInt(r.RoundSkipped), planJSON,
		r.WinnerProposalID, r.SummaryText, r.SummaryAuthor, r.SummaryAt, r.ResultJSON,
		t.tenantID, r.ID)
	return err
}

// =============================================================================
// agents
// =============================================================================

const agentCols = `tenant_id, room_id, id, alias, model, token_hash, joined_at, ord,
	quota_tokens, tokens_read, tokens_written, tokens_total, read_cursor, final_stance, last_stance_round,
	last_seen_at`

func (t *Tx) InsertAgent(a *domain.Agent) error {
	q := `INSERT INTO agents (` + agentCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := t.exec(q, t.tenantID, a.RoomID, a.ID, a.Alias, a.Model, a.TokenHash, a.JoinedAt,
		a.Ord, a.QuotaTokens, a.TokensRead, a.TokensWritten, a.TokensTotal, a.ReadCursor,
		a.FinalStance, a.LastStanceRound, a.LastSeenAt)
	return err
}

// ListAgents 按入场顺序返回房间内所有 agent。
func (t *Tx) ListAgents(roomID string) ([]*domain.Agent, error) {
	rows, err := t.query(`SELECT `+agentCols+` FROM agents WHERE tenant_id = ? AND room_id = ? ORDER BY ord ASC`,
		t.tenantID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Agent
	for rows.Next() {
		var a domain.Agent
		if err := rows.Scan(&a.TenantID, &a.RoomID, &a.ID, &a.Alias, &a.Model, &a.TokenHash,
			&a.JoinedAt, &a.Ord, &a.QuotaTokens, &a.TokensRead, &a.TokensWritten, &a.TokensTotal,
			&a.ReadCursor, &a.FinalStance, &a.LastStanceRound, &a.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (t *Tx) SaveAgent(a *domain.Agent) error {
	q := `UPDATE agents SET alias = ?, model = ?, token_hash = ?, quota_tokens = ?,
		tokens_read = ?, tokens_written = ?, tokens_total = ?, read_cursor = ?,
		final_stance = ?, last_stance_round = ?, last_seen_at = ?
		WHERE tenant_id = ? AND room_id = ? AND id = ?`
	_, err := t.exec(q, a.Alias, a.Model, a.TokenHash, a.QuotaTokens, a.TokensRead,
		a.TokensWritten, a.TokensTotal, a.ReadCursor, a.FinalStance, a.LastStanceRound, a.LastSeenAt,
		t.tenantID, a.RoomID, a.ID)
	return err
}

// FindAgentByToken 是凭证三元组校验的唯一实现：
// 必须 tenant + room + token_hash 三者同时匹配，少一个都查不到。
func (t *Tx) FindAgentByToken(roomID, tokenHash string) (*domain.Agent, error) {
	row, err := t.queryRow(`SELECT `+agentCols+` FROM agents
		WHERE tenant_id = ? AND room_id = ? AND token_hash = ?`, t.tenantID, roomID, tokenHash)
	if err != nil {
		return nil, err
	}
	var a domain.Agent
	err = row.Scan(&a.TenantID, &a.RoomID, &a.ID, &a.Alias, &a.Model, &a.TokenHash, &a.JoinedAt,
		&a.Ord, &a.QuotaTokens, &a.TokensRead, &a.TokensWritten, &a.TokensTotal, &a.ReadCursor,
		&a.FinalStance, &a.LastStanceRound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// =============================================================================
// messages
// =============================================================================

const msgCols = `tenant_id, room_id, seq, round, agent_id, alias, model, text, reply_to,
	stance, mentions_json, ts, op_key, support_proposal_id`

// ListMessages 返回 seq > since 的消息，按 seq 升序。
func (t *Tx) ListMessages(roomID string, since int) ([]*domain.Message, error) {
	rows, err := t.query(`SELECT `+msgCols+` FROM messages
		WHERE tenant_id = ? AND room_id = ? AND seq > ? ORDER BY seq ASC`, t.tenantID, roomID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Message
	for rows.Next() {
		var m domain.Message
		var replyTo sql.NullInt64
		var mentions string
		if err := rows.Scan(&m.TenantID, &m.RoomID, &m.Seq, &m.Round, &m.AgentID, &m.Alias,
			&m.Model, &m.Text, &replyTo, &m.Stance, &mentions, &m.TS, &m.OpKey, &m.SupportProposalID); err != nil {
			return nil, err
		}
		if replyTo.Valid {
			v := int(replyTo.Int64)
			m.ReplyTo = &v
		}
		if mentions != "" {
			_ = json.Unmarshal([]byte(mentions), &m.Mentions)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (t *Tx) InsertMessage(m *domain.Message) error {
	q := `INSERT INTO messages (` + msgCols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	mentionsJSON, err := json.Marshal(m.Mentions)
	if err != nil {
		return err
	}
	var replyTo any
	if m.ReplyTo != nil {
		replyTo = *m.ReplyTo
	}
	// ★ support_proposal_id 必须真的写进去：迁移 003 加了这一列，但 store 的列清单一直
	//   没带上它，于是「收尾轮我支持哪个方案」在落库时被静默丢掉，读回来永远是空串。
	//   实证：一次真实演练里 positions 拿不到表态、立场矩阵退回"最长那条"（第 1 轮）。
	_, err = t.exec(q, t.tenantID, m.RoomID, m.Seq, m.Round, m.AgentID, m.Alias, m.Model,
		m.Text, replyTo, m.Stance, string(mentionsJSON), m.TS, m.OpKey, m.SupportProposalID)
	return err
}

// =============================================================================
// op_keys（幂等表）
// =============================================================================

func (t *Tx) FindOpKey(roomID, agentID, opKey string) (int, bool, error) {
	row, err := t.queryRow(`SELECT seq FROM op_keys
		WHERE tenant_id = ? AND room_id = ? AND agent_id = ? AND op_key = ?`,
		t.tenantID, roomID, agentID, opKey)
	if err != nil {
		return 0, false, err
	}
	var seq int
	err = row.Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (t *Tx) PutOpKey(roomID, agentID, opKey string, seq int) error {
	_, err := t.exec(`INSERT INTO op_keys (tenant_id, room_id, agent_id, op_key, seq)
		VALUES (?,?,?,?,?)`, t.tenantID, roomID, agentID, opKey, seq)
	return err
}

// TouchAgent 只更新活跃时间。
//
// 为什么要单独一条：聚合式落库只在"状态有变化"时才 saveAgg，
// 而心跳刷新活跃时间时 tick 往往没变化 —— 如果顺手写在聚合里就永远存不下去，
// 结果活着的人会被判成掉线。这是一条与业务状态无关的轻量写。
func (t *Tx) TouchAgent(roomID, agentID string, ts int64) error {
	_, err := t.exec(`UPDATE agents SET last_seen_at = ?
		WHERE tenant_id = ? AND room_id = ? AND id = ?`, ts, t.tenantID, roomID, agentID)
	return err
}

// FindAgentByAlias 按 alias 找成员 —— reclaim（认回）用。
//
// 注意：调用方必须已经校验过房间密码。只凭 alias 认回等于"谁猜到名字就能接管"，
// 是绝对不能放开的边界。
func (t *Tx) FindAgentByAlias(roomID, alias string) (*domain.Agent, error) {
	row, err := t.queryRow(`SELECT `+agentCols+` FROM agents
		WHERE tenant_id = ? AND room_id = ? AND alias = ? ORDER BY ord ASC LIMIT 1`,
		t.tenantID, roomID, alias)
	if err != nil {
		return nil, err
	}
	var a domain.Agent
	err = row.Scan(&a.TenantID, &a.RoomID, &a.ID, &a.Alias, &a.Model, &a.TokenHash, &a.JoinedAt,
		&a.Ord, &a.QuotaTokens, &a.TokensRead, &a.TokensWritten, &a.TokensTotal, &a.ReadCursor,
		&a.FinalStance, &a.LastStanceRound, &a.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// RotateAgentToken 换发新 token（认回）：旧哈希立即失效，agentId 不变。
//
// now 由调用方传（docs/09 §4.2-2）：认回要写 last_seen_at，而这个时间必须与
// 引擎当次 op 的 now 同一个来源，否则离线重放会拿到一个读真实时钟的输入。
func (t *Tx) RotateAgentToken(roomID, agentID, newTokenHash string, now int64) error {
	_, err := t.exec(`UPDATE agents SET token_hash = ?, last_seen_at = ?
		WHERE tenant_id = ? AND room_id = ? AND id = ?`,
		newTokenHash, now, t.tenantID, roomID, agentID)
	return err
}

// DeleteTokenDirectoryByAgent 清掉某个 agent 旧的 token 归属映射。
// 认回换发 token 时必须调，否则旧 token 在审计表里还挂着。
func (t *Tx) DeleteTokenDirectoryByAgent(roomID, agentID string) error {
	_, err := t.exec(`DELETE FROM token_directory
		WHERE tenant_id = ? AND room_id = ? AND agent_id = ?`, t.tenantID, roomID, agentID)
	return err
}

// =============================================================================
// proposals / votes（v2 会议流程）
// =============================================================================

const proposalCols = `tenant_id, room_id, id, seq, agent_id, alias, model, title, body, created_at,
	supersedes, superseded_by`

// ListProposals 按提交顺序返回房间内全部方案（含已被取代的旧版本，供追溯）。
func (t *Tx) ListProposals(roomID string) ([]*domain.Proposal, error) {
	rows, err := t.query(`SELECT `+proposalCols+` FROM proposals
		WHERE tenant_id = ? AND room_id = ? ORDER BY seq ASC`, t.tenantID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Proposal
	for rows.Next() {
		var p domain.Proposal
		if err := rows.Scan(&p.TenantID, &p.RoomID, &p.ID, &p.Seq, &p.AgentID, &p.Alias,
			&p.Model, &p.Title, &p.Body, &p.CreatedAt, &p.Supersedes, &p.SupersededBy); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// UpsertProposal 写入方案。**正文一旦写入绝不修改**（可追溯）；唯一允许变化的是
// superseded_by —— 它记录"这个版本被哪个新版本取代了"，且只允许从 ” 变成某个 ID。
func (t *Tx) UpsertProposal(p *domain.Proposal) error {
	_, err := t.exec(`INSERT INTO proposals (tenant_id, room_id, id, seq, agent_id, alias, model,
		title, body, created_at, supersedes, superseded_by) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (tenant_id, room_id, id) DO UPDATE SET superseded_by = excluded.superseded_by
		WHERE proposals.superseded_by = ''`,
		t.tenantID, p.RoomID, p.ID, p.Seq, p.AgentID, p.Alias, p.Model, p.Title, p.Body, p.CreatedAt,
		p.Supersedes, p.SupersededBy)
	return err
}

// RetargetVotes 把投给 from 的票改记到 to（旧版本被新版本取代时的迁移）。
// 不这么做的话，已经投出的票会落在一个"不再采纳"的方案上，等于白投。
func (t *Tx) RetargetVotes(roomID, from, to string) error {
	_, err := t.exec(`UPDATE votes SET proposal_id = ?
		WHERE tenant_id = ? AND room_id = ? AND proposal_id = ?`, to, t.tenantID, roomID, from)
	return err
}

const voteCols = `tenant_id, room_id, agent_id, proposal_id, final_stance, reason, created_at, redirected_from`

// ListVotes 返回房间内全部票。
func (t *Tx) ListVotes(roomID string) ([]*domain.Vote, error) {
	rows, err := t.query(`SELECT `+voteCols+` FROM votes
		WHERE tenant_id = ? AND room_id = ? ORDER BY agent_id ASC`, t.tenantID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Vote
	for rows.Next() {
		var v domain.Vote
		if err := rows.Scan(&v.TenantID, &v.RoomID, &v.AgentID, &v.ProposalID,
			&v.FinalStance, &v.Reason, &v.CreatedAt, &v.RedirectedFrom); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// UpsertVote 写票。**已存在则覆盖**（每人 1 票，可改票，不重复计票）。
func (t *Tx) UpsertVote(v *domain.Vote) error {
	_, err := t.exec(`INSERT INTO votes (tenant_id, room_id, agent_id, proposal_id, final_stance,
		reason, created_at, redirected_from) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (tenant_id, room_id, agent_id) DO UPDATE SET
			proposal_id = excluded.proposal_id,
			final_stance = excluded.final_stance,
			reason = excluded.reason,
			created_at = excluded.created_at,
			redirected_from = excluded.redirected_from`,
		t.tenantID, v.RoomID, v.AgentID, v.ProposalID, v.FinalStance, v.Reason, v.CreatedAt, v.RedirectedFrom)
	return err
}

// =============================================================================
// 辅助
// =============================================================================

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodePlan(p *domain.Plan) (string, error) {
	if p == nil {
		return "", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodePlan(s string) (*domain.Plan, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var p domain.Plan
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RoomCounts 是"只要数字"的轻量视图：状态页/巡检用，不必把全部消息读进内存。
//
// 一次走三条带 tenant 前缀的计数查询 + 房间行，比完整载入聚合少一个数量级的分配。
type RoomCounts struct {
	Messages  int
	Proposals int
	Votes     int
}

func (t *Tx) RoomCounts(roomID string) (RoomCounts, error) {
	var c RoomCounts
	rows := []struct {
		q   string
		dst *int
	}{
		{`SELECT COUNT(*) FROM messages WHERE tenant_id = ? AND room_id = ?`, &c.Messages},
		{`SELECT COUNT(*) FROM proposals WHERE tenant_id = ? AND room_id = ?`, &c.Proposals},
		{`SELECT COUNT(*) FROM votes WHERE tenant_id = ? AND room_id = ?`, &c.Votes},
	}
	for _, r := range rows {
		row, err := t.queryRow(r.q, t.tenantID, roomID)
		if err != nil {
			return c, err
		}
		if err := row.Scan(r.dst); err != nil {
			return c, err
		}
	}
	return c, nil
}
