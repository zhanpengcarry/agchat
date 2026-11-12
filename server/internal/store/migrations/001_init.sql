-- =============================================================================
-- agchat 初始 schema
--
-- ★ 数据隔离的第一层：**每张业务表都有 tenant_id NOT NULL**，
--   且所有主键与索引都以 tenant_id 打头（复合前缀）。
--   这样做有两个后果，都是故意的：
--     1. 任何一条查询只要带上 tenant_id 前缀索引就是范围扫描，不会全表扫；
--     2. 将来上 Postgres 开 RLS 时，策略 `tenant_id = current_setting('app.tenant_id')`
--        能命中索引，不会退化成全表过滤。
--
-- ★ 注意 tenants 表本身没有 tenant_id 列 —— 它就是租户。
--   它是唯一允许在"无租户作用域"的事务外写入的表（见 store.NewTenant）。
-- =============================================================================

-- 租户：一次 POST /rooms 建一个匿名租户。
CREATE TABLE IF NOT EXISTS tenants (
  id         TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);

-- 房间。所有状态都在这张表上。
CREATE TABLE IF NOT EXISTS rooms (
  tenant_id          TEXT    NOT NULL,
  id                 TEXT    NOT NULL,
  topic              TEXT    NOT NULL,
  rules              TEXT    NOT NULL DEFAULT '',
  state              TEXT    NOT NULL,              -- briefing|debating|converging|ended
  round              INTEGER NOT NULL DEFAULT 0,
  max_rounds         INTEGER NOT NULL,
  max_agents         INTEGER NOT NULL,
  minutes_limit      INTEGER NOT NULL,
  token_budget       INTEGER NOT NULL,              -- 预算上限（est_tokens）
  budget_used        INTEGER NOT NULL DEFAULT 0,    -- 读+写累计
  budget_read        INTEGER NOT NULL DEFAULT 0,
  budget_written     INTEGER NOT NULL DEFAULT 0,
  budget_overrun     INTEGER NOT NULL DEFAULT 0,    -- 1 = 收敛轮突破总预算（有结论优先）
  password_hash      TEXT    NOT NULL,              -- sha256，明文只返回一次
  admin_key_hash     TEXT    NOT NULL,              -- sha256，明文只返回一次
  created_at         INTEGER NOT NULL,
  started_at         INTEGER NOT NULL DEFAULT 0,    -- 0 = 未开始
  ended_at           INTEGER NOT NULL DEFAULT 0,    -- 0 = 未结束
  briefing_deadline  INTEGER NOT NULL,
  end_reason         TEXT    NOT NULL DEFAULT '',
  convergence_reason TEXT    NOT NULL DEFAULT '',
  seq                INTEGER NOT NULL DEFAULT 0,    -- 房间内消息序号
  plan_json          TEXT    NOT NULL DEFAULT '',   -- 当前轮发言队列
  result_json        TEXT    NOT NULL DEFAULT '',   -- 结束时冻结的结论
  PRIMARY KEY (tenant_id, id)
);
-- 复合索引：租户前缀。列房间列表、按时间排序都走它。
CREATE INDEX IF NOT EXISTS idx_rooms_tenant_created ON rooms (tenant_id, created_at);
-- 后台 tick 需要扫描"未结束的房间"：仍然以 tenant_id 打头，保证不越界。
CREATE INDEX IF NOT EXISTS idx_rooms_tenant_state ON rooms (tenant_id, state);

-- agent 成员。人类没有记录：本产品人类没有发言通道。
CREATE TABLE IF NOT EXISTS agents (
  tenant_id         TEXT    NOT NULL,
  room_id           TEXT    NOT NULL,
  id                TEXT    NOT NULL,
  alias             TEXT    NOT NULL,
  model             TEXT    NOT NULL,
  token_hash        TEXT    NOT NULL,               -- sha256(agentToken)
  joined_at         INTEGER NOT NULL,
  ord               INTEGER NOT NULL,               -- 入场顺序，决定第 1 轮次序
  quota_tokens      INTEGER NOT NULL,               -- 展示用配额 = budget/maxAgents
  tokens_read       INTEGER NOT NULL DEFAULT 0,
  tokens_written    INTEGER NOT NULL DEFAULT 0,
  tokens_total      INTEGER NOT NULL DEFAULT 0,
  read_cursor       INTEGER NOT NULL DEFAULT 0,     -- 已读到的 seq
  final_stance      TEXT    NOT NULL DEFAULT '',    -- 收敛轮投票
  last_stance_round INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, room_id, id)
);
-- token 鉴权走这条：tenant + room + token_hash 三元组全匹配。
CREATE INDEX IF NOT EXISTS idx_agents_tenant_room_token ON agents (tenant_id, room_id, token_hash);

-- 发言。
CREATE TABLE IF NOT EXISTS messages (
  tenant_id     TEXT    NOT NULL,
  room_id       TEXT    NOT NULL,
  seq           INTEGER NOT NULL,
  round         INTEGER NOT NULL,
  agent_id      TEXT    NOT NULL,
  alias         TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  text          TEXT    NOT NULL,
  reply_to      INTEGER,                            -- NULL = 不是回应
  stance        TEXT    NOT NULL DEFAULT '',        -- 收敛轮才有值
  mentions_json TEXT    NOT NULL DEFAULT '[]',      -- @ 到的 agentId
  ts            TEXT    NOT NULL,
  op_key        TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, room_id, seq)
);
-- 读消息走这条：WHERE tenant_id = ? AND room_id = ? AND seq > ?
CREATE INDEX IF NOT EXISTS idx_messages_tenant_room_seq ON messages (tenant_id, room_id, seq);

-- 幂等键：同 (tenant, room, agent, opKey) 只落一条消息。
CREATE TABLE IF NOT EXISTS op_keys (
  tenant_id TEXT    NOT NULL,
  room_id   TEXT    NOT NULL,
  agent_id  TEXT    NOT NULL,
  op_key    TEXT    NOT NULL,
  seq       INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, room_id, agent_id, op_key)
);

-- =============================================================================
-- ★ 房间号 -> 租户 的全局路由表（唯一一张"无租户作用域"的查询对象）
--
-- 为什么必须有它：对外契约里客户端只握有 roomId（没有 tenantId）。
-- 服务端要在进入租户作用域之前，先把 roomId 翻译成 tenantId。
--
-- 为什么它不危险：这张表只有两列映射，不含任何议题/消息/凭证。
-- 而且它只用于**定位**，定位之后所有读写都必须经过 store.WithTenant，
-- 凭证三元组（tenant + room + token）仍然要在租户事务内校验。
-- 换句话说：偷到 roomId 只能偷到"这个房间属于某个租户"，偷不到内容。
--
-- 访问入口只有一个：store.ResolveRoom。代码评审时看这个名字就够了。
-- =============================================================================
CREATE TABLE IF NOT EXISTS room_directory (
  room_id    TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
