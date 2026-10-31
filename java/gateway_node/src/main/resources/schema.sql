-- Zone Directory Service tables
-- Run against the 'mmorpg' database

CREATE TABLE IF NOT EXISTS zone_config (
    zone_id        BIGINT PRIMARY KEY,
    name           VARCHAR(64) NOT NULL,
    manual_status  INT NOT NULL DEFAULT 0 COMMENT '0=OPEN, 1=MAINTENANCE, 2=CLOSED, 3=PREVIEW',
    capacity       INT DEFAULT 5000,
    maintenance_msg VARCHAR(256) DEFAULT '',
    open_time      DATETIME DEFAULT NULL,
    recommended    BIT(1) DEFAULT 0,
    sort_order     INT DEFAULT 0,
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS zone_whitelist (
    id         BIGINT PRIMARY KEY,
    zone_id    BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    note       VARCHAR(128) DEFAULT '',
    UNIQUE KEY uk_zone_account (zone_id, account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS announcement (
    id         BIGINT PRIMARY KEY,
    title      VARCHAR(128) NOT NULL,
    content    TEXT,
    type       VARCHAR(32) NOT NULL DEFAULT 'notice' COMMENT 'notice/maintenance/update',
    start_time DATETIME DEFAULT NULL,
    end_time   DATETIME DEFAULT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Seed default zone
INSERT IGNORE INTO zone_config (zone_id, name, manual_status, capacity, recommended, sort_order)
VALUES (1, 'zone-1', 0, 5000, 1, 1);
