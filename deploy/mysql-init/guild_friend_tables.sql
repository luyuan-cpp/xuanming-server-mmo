-- Guild service tables
-- 由 mysql 镜像 initdb 执行,落在 MYSQL_DATABASE(= mmorpg)这个共享库(本文件无 USE 语句,靠 entrypoint 的 --database 定库);
-- 存量库升级需在停写窗口手工整体执行本文件。

-- ⚠ 2026-09-18 起本文件**只剩 guild**:friend 已按 port-decisions D-14 迁到独占库 mmorpg_friend。
--   - 建库:deploy/mysql-init/00_init_zone_dbs.sql(唯一登记处,D-14 第 5 条);
--   - 建表:friend / friend_request / friend_block / friend_capacity 由 go/schemamigrate 按
--     proto/friend/friend_table.proto 建(friend 启动期 AutoMigrate,或 K8s 的 friend-migrate Job)。
--   所以本文件里原有的三段 friend CREATE TABLE 已删除。**不要加回来**:两处都建表会让 schemamigrate
--   把 initdb 建出来的旧结构判成类型漂移(退出码 4,需人工),friend 直接起不来。
--   同时删掉的还有 procedure 里 friend_capacity 的回填段与写 `friend_capacity_backfill_v1` 门禁行的语句:
--   那道"就绪闸"已按 **D-10 修订(2026-09-18)** 退役(未上线、无存量好友数据,容量计数由新库建表时从零开始,
--   不需要回填,也就不需要门禁)。回填段必须与建表段同批删除 —— 只删建表不删回填,空卷 initdb 会对
--   不存在的 friend / friend_capacity 执行 UPDATE,整个脚本失败,连 guild 的表都建不出来。
--   `guild_schema_migration` 表本身保留:guild 的积分迁移门禁(guild_score_redis_backfill_v1)还在用它。

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

  -- 这里原本是 friend_capacity 的回填段(门禁行 friend_capacity_backfill_v1 + 全表归零 + 按 friend 重算),
  -- 2026-09-18 随 friend 迁往独占库 mmorpg_friend 一并删除(见文件头注释;门禁按 D-10 修订退役)。
  -- 这个 COMMIT 保留(它原本兼着两个作用,只有"提交 friend 门禁行"那一半随回填段作废):
  -- 上面 guild_member 的补齐 membership / 改 role 是普通 DML,而它们之后唯一的 DDL(加 uk_player)
  -- 只在索引缺失时才跑。重跑场景下索引已存在 = 那之后没有任何隐式提交,调用方若是 autocommit=0,
  -- guild 的修复结果会悬在未提交事务里。空事务上 COMMIT 是无副作用的 no-op,删掉它是在删一层保护。
  COMMIT;

  INSERT IGNORE INTO guild_schema_migration (migration_key, state)
  VALUES ('guild_score_redis_backfill_v1', 'pending');
END$$
DELIMITER ;

CALL migrate_guild_friend_schema();
DROP PROCEDURE migrate_guild_friend_schema;
