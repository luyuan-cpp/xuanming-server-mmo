-- Friend service tables(留在 mmorpg 库)。
-- 帮会表已迁入独占库 mmorpg_guild,由 proto/guild/guild_db.proto + go/schemamigrate 建
-- (port-decisions D-14 §8 修订;设计见 docs/design/guild-phase2/01-storage.md)。

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
