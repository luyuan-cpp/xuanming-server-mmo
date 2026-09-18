-- Create per-zone databases and grant appuser access.
-- Docker MYSQL_USER only auto-grants on MYSQL_DATABASE (mmorpg),
-- so zone databases need explicit grants.

CREATE DATABASE IF NOT EXISTS zone_1_db;
GRANT ALL PRIVILEGES ON `zone_1_db`.* TO 'appuser'@'%';

-- zone_2_db is used by local two-zone stress tests (`dev start-zones`).
CREATE DATABASE IF NOT EXISTS zone_2_db;
GRANT ALL PRIVILEGES ON `zone_2_db`.* TO 'appuser'@'%';

-- testdb is the local "global" database of data_service
-- (go/data_service/etc/data_service.yaml SnapshotMySQL.DBName). data_service
-- creates transaction_log / player_snapshot / rollback_audit_log / id_segment
-- there at startup (Schema.AutoMigrate) or via `data_service -migrate`; if the
-- database is missing or appuser has no grant, startup logs "schema auto-migrate
-- failed" and AllocateIdSegment (login PlayerId segments) is unavailable.
-- K8s does not use this name: k8s_deploy.ps1 generates 02_k8s_global_db.sql for
-- the cluster-global database instead. NOTE: initdb scripts only run on an empty
-- data volume; an already-initialised local MySQL will not pick this up -- run the
-- two statements below by hand or recreate the volume.
CREATE DATABASE IF NOT EXISTS testdb;
GRANT ALL PRIVILEGES ON `testdb`.* TO 'appuser'@'%';

-- mmorpg_trade:聚宝斋 trade 服务独占库(go/trade/etc/trade.yaml 的 MySQL.DBName)。
-- 按 port-decisions D-14,这里只建库 + 授权;表以 proto/trade/trade_table.proto 为源,
-- 由 go/schemamigrate 建(trade 启动期 Schema.AutoMigrate,或 `trade -f etc/trade.yaml -migrate`),
-- 不要往 mysql-init 里加业务表。appuser 没有全局 CREATE 权限,库必须在 trade 启动前就存在。
-- 注意:与上面 testdb 相同,已初始化过的本地 MySQL 数据卷不会重跑 initdb,
-- 存量卷需要用 root 手工执行下面两句(再 FLUSH PRIVILEGES),或重建数据卷。
-- 本机一键启动(tools/scripts/start_game.ps1)在 MySQL 就绪后预检本库,库不在或 appuser 无权时
-- 跳过 trade 并打印补建命令,不拖垮整个启动。
-- K8s:k8s_deploy.ps1 的 mysql-init-sql ConfigMap 会原样带入本文件,新 PVC 首次 initdb 时同样建库;
-- 已有数据的 PVC 不会重跑,手工补建步骤见 deploy/k8s/README.md
-- (D-14 第 5 条:只登记这一处,不要在 k8s_deploy.ps1 另生成)。
CREATE DATABASE IF NOT EXISTS mmorpg_trade DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON `mmorpg_trade`.* TO 'appuser'@'%';

-- mmorpg_guild:帮会 guild 服务独占库(go/guild/etc/guild.yaml MySQL.DataSource 的库名;
-- go/guild/internal/config.Validate 断言 DSN 库名等于它)。按 port-decisions D-14(§8 已修订:
-- 帮会表一并迁入),这里只建库 + 授权;表以 proto/guild/guild_db.proto 为源,由 go/schemamigrate
-- 建(guild 启动期 Schema.AutoMigrate,或 `guild -f etc/guild.yaml -migrate`),不要往 mysql-init 加帮会表。
-- appuser 没有全局 CREATE 权限,库必须在 guild 启动前就存在。
-- 已初始化过的本地数据卷不会重跑 initdb:用 root 手工执行下面两句(再 FLUSH PRIVILEGES),或重建数据卷;
-- tools/scripts/start_game.ps1 在 MySQL 就绪后预检本库,不就绪时跳过 guild 并打印补建命令。
-- K8s:mysql-init-sql ConfigMap 原样带入本文件,新 PVC 首次 initdb 建库;已有 PVC 手工补建(见 deploy/k8s/README.md)。
CREATE DATABASE IF NOT EXISTS mmorpg_guild DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON `mmorpg_guild`.* TO 'appuser'@'%';

FLUSH PRIVILEGES;
