-- =============================================================================
-- 002 · token 路由/审计索引
--
-- 与 room_directory 同类：只做"凭据归属"的定位与审计，不含任何内容。
--
-- 为什么要它：当请求带着一个不属于本房间的 token 打过来时，服务要用它写审计日志
-- （"这枚 token 属于哪个租户"）。若改成去 agents 表里跨租户查 token_hash，
-- 那就是一条**无租户作用域、直接读业务表**的查询 —— 恰好是 store 层明令禁止的形态。
-- 所以把这条映射单独放一张窄表，和 agents 的业务字段彻底分开。
--
-- 访问入口只有一个：store.LookupTokenTenant，且**只在 404 失败路径上调用**
-- （正常请求永远不会碰它，不为它付任何开销）。
-- =============================================================================

CREATE TABLE IF NOT EXISTS token_directory (
  token_hash TEXT PRIMARY KEY,   -- 只存哈希，明文 token 从不落库
  tenant_id  TEXT NOT NULL,
  room_id    TEXT NOT NULL,
  agent_id   TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_token_directory_tenant ON token_directory (tenant_id, room_id);
