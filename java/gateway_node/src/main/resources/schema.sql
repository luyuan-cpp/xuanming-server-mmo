-- Zone Directory Service tables
-- Run against the 'mmorpg' database
-- 三张表显式写 COLLATE:只写 CHARSET 时,MySQL 取 utf8mb4 的缺省排序规则 utf8mb4_0900_ai_ci,TiDB 取 utf8mb4_bin,
-- 两边的大小写与比较语义会不一样(docs/design/tidb-phase1-and-launch-readiness.md §4 第 10 项)。这里写的就是 MySQL 上
-- 现在实际用的值,MySQL 档行为不变。deploy/mysql-init/gateway_tables.sql 里的同名三张表与这里保持一致。

CREATE TABLE IF NOT EXISTS zone_config (
    zone_id        BIGINT PRIMARY KEY,
    name           VARCHAR(64) NOT NULL,
    manual_status  INT NOT NULL DEFAULT 0 COMMENT '0=OPEN, 1=MAINTENANCE, 2=CLOSED, 3=PREVIEW',
    capacity       INT DEFAULT 45000,
    maintenance_msg VARCHAR(256) DEFAULT '',
    open_time      DATETIME DEFAULT NULL,
    recommended    BIT(1) DEFAULT 0,
    sort_order     INT DEFAULT 0,
    create_role_closed BIT(1) NOT NULL DEFAULT 0 COMMENT '1 = 暂停创建新角色;只经 /admin/zones/{id}/create-role/close|open 改',
    role_capacity INT NOT NULL DEFAULT 0 COMMENT '本区角色上限(0 = 不设):角色总数到了就由本区网关关上 create_role_closed;只经 /admin/zones/{id}/role-capacity 改',
    version        BIGINT NOT NULL DEFAULT 1 COMMENT '行版本:每次写入加一,GET /admin/zones/{id} 以它作 ETag,整行 PUT 带 If-Match 比较并写',
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS zone_whitelist (
    id         BIGINT PRIMARY KEY,
    zone_id    BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    note       VARCHAR(128) DEFAULT '',
    UNIQUE KEY uk_zone_account (zone_id, account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS announcement (
    id         BIGINT PRIMARY KEY,
    title      VARCHAR(128) NOT NULL,
    content    TEXT,
    type       VARCHAR(32) NOT NULL DEFAULT 'notice' COMMENT 'notice/maintenance/update',
    start_time DATETIME DEFAULT NULL,
    end_time   DATETIME DEFAULT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Seed default zone
INSERT IGNORE INTO zone_config (zone_id, name, manual_status, capacity, recommended, sort_order)
VALUES (1, 'zone-1', 0, 45000, 1, 1);
