-- =============================================================================
-- 003 · v2 会议流程
--
-- 变更要点：
--   - 会议号 roomNo（9 位数字，对外心智），存 room_no 并在路由表上建唯一索引；
--   - 主席 moderator_id（第一个进场的人）；
--   - 单条输出上限 max_output_tokens、连续超时计数 chain_timeouts、阶段起始时间；
--   - 结尾产物：获胜方案、概述正文/作者/时间；
--   - 新表 proposals / votes（都带 tenant_id，走 WithTenant 作用域）。
-- =============================================================================

ALTER TABLE rooms ADD COLUMN room_no            TEXT    NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN moderator_id       TEXT    NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN max_output_tokens  INTEGER NOT NULL DEFAULT 800;
ALTER TABLE rooms ADD COLUMN chain_timeouts     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN phase_started_at   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN winner_proposal_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN summary_text       TEXT    NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN summary_author     TEXT    NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN summary_at         INTEGER NOT NULL DEFAULT 0;

-- 收尾轮要说"我支持哪个方案"
ALTER TABLE messages ADD COLUMN support_proposal_id TEXT NOT NULL DEFAULT '';

-- 会议号也要能路由到租户：扩展既有的全局路由表（与 room_id 并列）
ALTER TABLE room_directory ADD COLUMN room_no TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_room_directory_no ON room_directory (room_no) WHERE room_no <> '';

-- 方案：每人最多 2 个，提交后不可改。
CREATE TABLE IF NOT EXISTS proposals (
  tenant_id  TEXT    NOT NULL,
  room_id    TEXT    NOT NULL,
  id         TEXT    NOT NULL,              -- p_1, p_2 …
  seq        INTEGER NOT NULL,              -- 提交时房间消息序号（决定"最早提交"）
  agent_id   TEXT    NOT NULL,
  alias      TEXT    NOT NULL,
  model      TEXT    NOT NULL,
  title      TEXT    NOT NULL,
  body       TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, room_id, id)
);
CREATE INDEX IF NOT EXISTS idx_proposals_tenant_room ON proposals (tenant_id, room_id, seq);
CREATE INDEX IF NOT EXISTS idx_proposals_tenant_agent ON proposals (tenant_id, room_id, agent_id);

-- 投票：每人 1 票，可改（覆盖）。proposal_id 为空表示弃权。
-- final_stance 仅用于 §九 的降级场景（房间内没有任何方案时）。
CREATE TABLE IF NOT EXISTS votes (
  tenant_id    TEXT    NOT NULL,
  room_id      TEXT    NOT NULL,
  agent_id     TEXT    NOT NULL,
  proposal_id  TEXT    NOT NULL DEFAULT '',
  final_stance TEXT    NOT NULL DEFAULT '',
  reason       TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, room_id, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_votes_tenant_room ON votes (tenant_id, room_id);
