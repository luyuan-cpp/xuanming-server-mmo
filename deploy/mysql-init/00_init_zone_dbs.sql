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
-- 匹配与组队的 SQL 权威；表由 match -migrate 建，只建自己的库。
CREATE DATABASE IF NOT EXISTS mmorpg_match DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- 目录（account_role 等）和流水。本机 K=1：目录就是基名这一个库；K8s 由 02_k8s_global_db.sql 按 -DirectoryLogicalShards 建片库和流水库。
CREATE DATABASE IF NOT EXISTS mmorpg_global DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mmorpg_global_ledger DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- docker 调试栈的 data_service 用 testdb 当全局库基名。
CREATE DATABASE IF NOT EXISTS testdb_ledger DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
