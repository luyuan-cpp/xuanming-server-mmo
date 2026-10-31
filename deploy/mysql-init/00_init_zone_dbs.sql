-- 这里只预建具名业务库。所有runtime权限只来自deploy/sql/authority_runtime_role.sql，按每boot固定表manifest授予。
-- 不自动给旧appuser或MYSQL_USER授schema.*；已有账号宽权限必须在独立维护权限核验中撤回。
CREATE DATABASE IF NOT EXISTS zone_1_db;
CREATE DATABASE IF NOT EXISTS zone_2_db;
CREATE DATABASE IF NOT EXISTS testdb;
CREATE DATABASE IF NOT EXISTS mmorpg_trade DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_guild DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_friend DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_player_inbox DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_mail DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_rank DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_chat DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- 匹配与组队的 SQL 权威；表由 match -migrate 建（与 guild 互相核对对方的表）。
CREATE DATABASE IF NOT EXISTS mmorpg_match DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- 全局库（account_role 等），本机 data_service 与 match 共用，K8s 另由 02_k8s_global_db.sql 建同名库。
CREATE DATABASE IF NOT EXISTS mmorpg_global DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
