-- Guild & Friend service tables
-- Add to the main database or run separately for the guild/friend schema.

-- ── Guild ────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS guild (
  guild_id       BIGINT UNSIGNED NOT NULL,
  name           VARCHAR(64)     NOT NULL,
  leader_id      BIGINT UNSIGNED NOT NULL DEFAULT 0,
  level          INT UNSIGNED    NOT NULL DEFAULT 1,
  announcement   TEXT,
  create_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
  max_members    INT UNSIGNED    NOT NULL DEFAULT 50,
  zone_id        INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT 'zone the guild belongs to',
  score          BIGINT          NOT NULL DEFAULT 0 COMMENT 'rank score, authoritative copy; Redis ZSET is a rebuildable cache',
  PRIMARY KEY (guild_id),
  UNIQUE KEY uk_name (name),
  KEY idx_leader (leader_id),
  KEY idx_zone (zone_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='guild';

-- role 取值必须与 go/guild/internal/constants/constants.go 一致:
-- 0=member, 1=officer, 3=leader(旧注释 1/2/3 与代码不符,以代码为准)。
CREATE TABLE IF NOT EXISTS guild_member (
  guild_id    BIGINT UNSIGNED NOT NULL,
  player_id   BIGINT UNSIGNED NOT NULL,
  role        TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0=member,1=officer,3=leader (constants.go)',
  join_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
  last_active_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
  contribution   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (guild_id, player_id),
  UNIQUE KEY uk_player (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='guild_member';

CREATE TABLE IF NOT EXISTS guild_schema_migration (
  migration_key VARCHAR(96) NOT NULL,
  state         VARCHAR(16) NOT NULL DEFAULT 'pending',
  completed_at  TIMESTAMP NULL DEFAULT NULL,
  PRIMARY KEY (migration_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='guild schema/data migration gates';

-- ── Friend ───────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS friend (
  player_id        BIGINT UNSIGNED NOT NULL,
  friend_player_id BIGINT UNSIGNED NOT NULL,
  since_ms         BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (player_id, friend_player_id),
  KEY idx_friend (friend_player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='friend';

CREATE TABLE IF NOT EXISTS friend_request (
  from_player_id  BIGINT UNSIGNED NOT NULL,
  to_player_id    BIGINT UNSIGNED NOT NULL,
  request_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
  status          TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '1=pending,2=accepted,3=rejected',
  PRIMARY KEY (from_player_id, to_player_id),
  KEY idx_to_player (to_player_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='friend_request';

-- 好友上限的显式锁行。AcceptFriend 锁双方 capacity 行，不依赖
-- COUNT ... FOR UPDATE 在特定隔离级别下是否产生 gap lock。
CREATE TABLE IF NOT EXISTS friend_capacity (
  player_id    BIGINT UNSIGNED NOT NULL,
  friend_count INT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='authoritative friend-list capacity counters';

-- ── Idempotent expand/backfill migration ─────────────────────
--
-- docker-entrypoint-initdb.d 只在空数据卷执行；存量库升级时必须在停写窗口
-- 手工执行**本文件整体**。下面的 procedure 可重复运行：已有列/索引不会重复加。
-- 若检测到同一 player_id 已属于多个公会会 fail-closed，先人工裁决归属后重跑；
-- 脚本不会擅自删除任何玩家的 membership。

DROP PROCEDURE IF EXISTS migrate_guild_friend_schema;
DELIMITER $$
CREATE PROCEDURE migrate_guild_friend_schema()
BEGIN
  DECLARE duplicate_players BIGINT DEFAULT 0;
  DECLARE leader_conflicts BIGINT DEFAULT 0;
  DECLARE EXIT HANDLER FOR SQLEXCEPTION
  BEGIN
    ROLLBACK;
    RESIGNAL;
  END;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'guild' AND COLUMN_NAME = 'score'
  ) THEN
    ALTER TABLE guild
      ADD COLUMN score BIGINT NOT NULL DEFAULT 0
      COMMENT 'rank score, authoritative copy; Redis ZSET is a rebuildable cache';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'guild_member' AND COLUMN_NAME = 'last_active_ms'
  ) THEN
    ALTER TABLE guild_member
      ADD COLUMN last_active_ms BIGINT UNSIGNED NOT NULL DEFAULT 0;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'guild_member' AND COLUMN_NAME = 'contribution'
  ) THEN
    ALTER TABLE guild_member
      ADD COLUMN contribution BIGINT UNSIGNED NOT NULL DEFAULT 0;
  END IF;

  UPDATE guild_member
  SET last_active_ms = join_time_ms
  WHERE last_active_ms = 0 AND join_time_ms > 0;

  ALTER TABLE guild_member ALTER COLUMN role SET DEFAULT 0;

  -- 旧 CreateGuild 随后调用的 SetPlayerGuild 把 role 硬编码为 0；且列缺失时
  -- 该调用会失败却仍向玩家返回成功，留下“有 guild 行、无会长 membership”。
  -- leader_id 是公会自身的权威字段：无歧义时补齐 membership 并修正为 role=3；
  -- 同一玩家领导多个公会或已属于别会时无法自动裁决，必须 fail-closed。
  SELECT COUNT(*) INTO leader_conflicts
  FROM (
    SELECT leader_id
    FROM guild
    GROUP BY leader_id
    HAVING COUNT(*) > 1
  ) AS duplicate_leaders;

  SELECT leader_conflicts + COUNT(*) INTO leader_conflicts
  FROM guild AS g
  JOIN guild_member AS gm
    ON gm.player_id = g.leader_id AND gm.guild_id <> g.guild_id;

  IF leader_conflicts > 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'guild leaders have ambiguous cross-guild ownership; resolve before migration';
  END IF;

  INSERT INTO guild_member
    (guild_id, player_id, role, join_time_ms, last_active_ms, contribution)
  SELECT g.guild_id, g.leader_id, 3, g.create_time_ms, g.create_time_ms, 0
  FROM guild AS g
  LEFT JOIN guild_member AS gm
    ON gm.guild_id = g.guild_id AND gm.player_id = g.leader_id
  WHERE gm.player_id IS NULL;

  UPDATE guild_member AS gm
  JOIN guild AS g
    ON g.guild_id = gm.guild_id AND g.leader_id = gm.player_id
  SET gm.role = 3
  WHERE gm.role <> 3;

  SELECT COUNT(*) INTO duplicate_players
  FROM (
    SELECT player_id
    FROM guild_member
    GROUP BY player_id
    HAVING COUNT(*) > 1
  ) AS duplicate_memberships;

  IF duplicate_players > 0 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'guild_member contains duplicate player memberships; resolve them before adding uk_player';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'guild_member' AND INDEX_NAME = 'uk_player'
  ) THEN
    ALTER TABLE guild_member ADD UNIQUE KEY uk_player (player_id);
  END IF;

  -- DDL 会隐式提交：friend_capacity 建表成功不代表历史 friend 已完成回填。
  -- 先把 durable gate 提交为 pending，让新服务在任何半迁移状态下 fail-closed；
  -- 随后的全表归零、权威重算和 ready 标记必须在同一个事务里提交。
  INSERT INTO guild_schema_migration (migration_key, state, completed_at)
  VALUES ('friend_capacity_backfill_v1', 'pending', NULL)
  ON DUPLICATE KEY UPDATE state = 'pending', completed_at = NULL;

  -- 不依赖调用方的 autocommit 设置：先把 fail-closed 状态单独持久化。
  COMMIT;

  START TRANSACTION;

  -- 整表先归零再按 friend 权威边重算，保证迁移脚本重跑也能修复已删到 0
  -- 的玩家；仅 UPSERT 非空 GROUP BY 会把旧的正数永久残留。若此段中断，
  -- EXIT HANDLER 回滚全部容量 DML，门禁保持 pending，绝不会暴露半份 0。
  UPDATE friend_capacity SET friend_count = 0;

  INSERT INTO friend_capacity (player_id, friend_count)
  SELECT player_id, COUNT(*)
  FROM friend
  GROUP BY player_id
  ON DUPLICATE KEY UPDATE friend_count = VALUES(friend_count);

  UPDATE guild_schema_migration
  SET state = 'ready', completed_at = CURRENT_TIMESTAMP
  WHERE migration_key = 'friend_capacity_backfill_v1';

  COMMIT;

  INSERT IGNORE INTO guild_schema_migration (migration_key, state)
  VALUES ('guild_score_redis_backfill_v1', 'pending');
END$$
DELIMITER ;

CALL migrate_guild_friend_schema();
DROP PROCEDURE migrate_guild_friend_schema;
