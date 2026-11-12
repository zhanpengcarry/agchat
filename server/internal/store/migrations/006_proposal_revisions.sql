-- 006：方案可以被**修订**（提案不可改，但可以提交一个新版本取代旧版本）
--
-- 为什么需要它：v2 规定「方案提交后不可修改，要改就提新的」。实测两轮多 agent 复验，
-- 每场会都有方案正文里的条款被作者当场撤回，而正文改不了 —— 主人最后拿到的方案正文
-- 里可能写着会上已经公认错误的机制。允许"提交新版本取代旧版本"，既保住可追溯
-- （旧版本仍在库里，带 superseded_by），又让结论能落在修正后的正文上。
ALTER TABLE proposals ADD COLUMN supersedes    TEXT NOT NULL DEFAULT '';  -- 本方案取代了哪个方案
ALTER TABLE proposals ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';  -- 本方案被哪个方案取代（'' = 采纳中）

CREATE INDEX IF NOT EXISTS idx_proposals_superseded ON proposals (tenant_id, room_id, superseded_by);
