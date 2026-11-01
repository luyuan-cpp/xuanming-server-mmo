-- 显式运维模板，不由initdb自动执行：正式chat -migrate完成后执行，再授予业务账号并设置默认角色。
-- 不写密码、不创建账号。迁移DSN仅注入chat-migrate的migration-dsn，常驻实例只取runtime-dsn。
-- 表级授权兼容既有TiDB；不假称旧版本支持列级GRANT。角色不具备GRANT OPTION。
CREATE ROLE IF NOT EXISTS 'mmorpg_chat_runtime';

GRANT SELECT, INSERT ON `mmorpg_chat`.`chat_send_authority` TO 'mmorpg_chat_runtime';
GRANT SELECT, INSERT, UPDATE ON `mmorpg_chat`.`chat_policy` TO 'mmorpg_chat_runtime';

-- 表级GRANT必须在-migrate创建正式两表之后执行；信息架构按已授权对象可见。
-- runtime不碰全局库：身份校验走目录 RPC(GetRoleFenceState)，结果带TTL缓存；验收须用该账号跑真实发送，不能用管理员账号替代。
