-- =============================================================================
-- 005 · 认回（reclaim）+ 空轮跳过
--
-- last_seen_at：agent 每次带 token 的请求都刷新它。
--   用途一：reclaim 时判断"这个 agent 是否还活着"（30s 内有请求 → 防双开）。
--   用途二：将来做在线状态。
--
-- empty_rounds：连续"整轮零发言"的轮数。整轮无人发言不推进 round（不白烧一轮），
--   连续 3 个空轮直接进投票，防无限空转。
-- round_skipped：当前这一轮是否被判为空轮。
-- =============================================================================

ALTER TABLE agents ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE rooms ADD COLUMN empty_rounds  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN round_skipped INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_agents_tenant_room_alias ON agents (tenant_id, room_id, alias);
