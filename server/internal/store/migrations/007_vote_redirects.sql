-- 007：记录"这一票本来投给谁、被自动改记到谁"
--
-- 为什么需要：方案可以被修订取代，投给旧版本时会自动迁移到最新版。
-- 迁移如果不留痕，结论里就看不见"这个人的本意是投旧版"——验证 agent 明确指出
-- 「票迁移在哈希域里不可见」。留一列，decisionSet 就能把它列出来。
ALTER TABLE votes ADD COLUMN redirected_from TEXT NOT NULL DEFAULT '';
