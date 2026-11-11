-- Zone Directory Service tables (gateway_node)
-- Applied automatically by docker-compose mysql-init
-- 三张业务表显式写 COLLATE,理由与取值同 java/gateway_node/src/main/resources/schema.sql 文件头:MySQL 档行为不变,
-- 换到 TiDB 时比较语义也一致。

USE mmorpg;

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

-- 写者闸门:每个逻辑库自带一份,闸门跟着库走(docs/design/horizontal-scale-logical-db.md §2)。
-- mmorpg 没有 Go 服务的 -migrate,所以写在这里。DDL 与 go/shared/authoritysql/catalog.go 逐字相同,TestGatewayTablesCarryWriterGate 钉住。
CREATE TABLE IF NOT EXISTS `route_meta` (
  `id` tinyint unsigned NOT NULL COMMENT 'pb:1',
  `generation` varchar(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:2',
  `writable` tinyint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:3',
  `writer_epoch` bigint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:4',
  `catalog_revision` bigint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:5',
  `freeze_run_id` varbinary(16) NOT NULL DEFAULT '' COMMENT 'pb:6',
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='route_meta';

CREATE TABLE IF NOT EXISTS writer_registry (
 writer_key BINARY(16) NOT NULL COMMENT 'pb:1',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:2',
 instance_id BINARY(16) NOT NULL COMMENT 'pb:3',
 sql_principal VARBINARY(288) NOT NULL COMMENT 'pb:4',
 registration_digest BINARY(32) NOT NULL COMMENT 'pb:5',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:6',
 registered_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 retirement_request_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:8',
 retirement_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:9',
 retired_ms BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:10',
 boot_seq BIGINT UNSIGNED NOT NULL COMMENT 'pb:11',
 PRIMARY KEY(writer_key), UNIQUE KEY uk_writer_principal_boot(sql_principal,boot_seq), UNIQUE KEY uk_writer_instance(instance_id),
 KEY idx_writer_state(state,writer_key),
 CONSTRAINT chk_writer_state CHECK(state IN (1,2)),
 CONSTRAINT chk_writer_boot_seq CHECK(boot_seq>0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_registry';

CREATE TABLE IF NOT EXISTS writer_slot (
 writer_key BINARY(16) NOT NULL COMMENT 'pb:1',
 slot TINYINT UNSIGNED NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 writer_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:5',
 lock_touch TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6',
 PRIMARY KEY(writer_key,slot),
 CONSTRAINT chk_writer_slot_number CHECK(slot<64),
 CONSTRAINT chk_writer_slot_state CHECK(state IN (1,2)),
 CONSTRAINT chk_writer_slot_epoch CHECK(writer_epoch>0),
 CONSTRAINT chk_writer_slot_touch CHECK(lock_touch IN (0,1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_slot';

CREATE TABLE IF NOT EXISTS writer_lifecycle_receipt (
 request_id BINARY(16) NOT NULL COMMENT 'pb:1',
 kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:4',
 request_proto MEDIUMBLOB NOT NULL COMMENT 'pb:5',
 previous_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',
 resulting_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 catalog_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',
 PRIMARY KEY(request_id), KEY idx_writer_lifecycle_request(kind,request_digest),
 KEY idx_writer_lifecycle_revision(catalog_revision,request_id),
 CONSTRAINT chk_writer_lifecycle_kind CHECK(kind IN (1,2)),
 CONSTRAINT chk_writer_lifecycle_epoch CHECK(previous_epoch>0 AND resulting_epoch>0),
 CONSTRAINT chk_writer_lifecycle_revision CHECK(catalog_revision>0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_lifecycle_receipt';

CREATE TABLE IF NOT EXISTS sql_writer_drain_run (
 operation_id BINARY(16) NOT NULL COMMENT 'pb:1',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',
 manifest_proto MEDIUMBLOB NOT NULL COMMENT 'pb:5',
 manifest_digest BINARY(32) NOT NULL COMMENT 'pb:6',
 phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:7',
 latest_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:8',
 frozen_request_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:9',
 updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 latest_receipt_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:11',
 PRIMARY KEY(operation_id),
 CONSTRAINT chk_writer_drain_attempt CHECK(attempt_revision>0),
 CONSTRAINT chk_writer_drain_phase CHECK(phase IN (1,3,4,5,6,7,8,9,10,11,12))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_drain_run';

CREATE TABLE IF NOT EXISTS sql_writer_freeze_ticket (
 ticket_id BINARY(16) NOT NULL COMMENT 'pb:1',
 operation_id BINARY(16) NOT NULL COMMENT 'pb:2',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:4',
 manifest_digest BINARY(32) NOT NULL COMMENT 'pb:5',
 non_sql_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:6',
 freeze_request_id BINARY(16) NOT NULL COMMENT 'pb:7',
 freeze_request_digest BINARY(32) NOT NULL COMMENT 'pb:8',
 not_before_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:9',
 expires_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:11',
 consumed_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:12',
 revocation_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:13',
 updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:14',
 authorization_kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:15',
 grant_receipt_id BINARY(16) NOT NULL COMMENT 'pb:16',
 grant_receipt_digest BINARY(32) NOT NULL COMMENT 'pb:17',
 PRIMARY KEY(ticket_id),
 UNIQUE KEY uk_writer_ticket_attempt(operation_id,attempt_revision),
 UNIQUE KEY uk_writer_ticket_freeze(freeze_request_id),
 CONSTRAINT chk_writer_ticket_state CHECK(state IN (1,2,3)),
 CONSTRAINT chk_writer_ticket_kind CHECK(authorization_kind IN (1,2)),
 CONSTRAINT chk_writer_ticket_window CHECK(not_before_ms>0 AND expires_ms>not_before_ms AND expires_ms-not_before_ms<=60000)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_freeze_ticket';

CREATE TABLE IF NOT EXISTS sql_writer_maintenance_receipt (
 request_id BINARY(16) NOT NULL COMMENT 'pb:1',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:2',
 run_id BINARY(16) NOT NULL COMMENT 'pb:3',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:4',
 before_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:5',
 after_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',
 catalog_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:8',
 plan_proto MEDIUMBLOB NOT NULL COMMENT 'pb:9',
 committed_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:11',
 run_phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:12',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:13',
 prior_receipt_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:14',
 prior_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:15',
 evidence MEDIUMBLOB NOT NULL COMMENT 'pb:16',
 receipt_digest BINARY(32) NOT NULL COMMENT 'pb:17',
 PRIMARY KEY(request_id), KEY idx_writer_receipt_run(run_id,attempt_revision),
 CONSTRAINT chk_writer_receipt_kind CHECK(kind BETWEEN 1 AND 8),
 CONSTRAINT chk_writer_receipt_phase CHECK(phase IN (0,1,2))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_maintenance_receipt';

CREATE TABLE IF NOT EXISTS db_move_sentinel (
 nonce BINARY(16) NOT NULL,
 run_id BINARY(16) NOT NULL,
 writer_epoch BIGINT UNSIGNED NOT NULL,
 written_ms BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(nonce)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='db_move_sentinel';

-- generation 只在第一次写入,之后一个字节都不动:写者资格的每个事务都核它。
INSERT INTO route_meta(id,generation,writable,writer_epoch,catalog_revision,freeze_run_id)
VALUES(1,LOWER(HEX(RANDOM_BYTES(16))),1,1,1,'') ON DUPLICATE KEY UPDATE id=id;
