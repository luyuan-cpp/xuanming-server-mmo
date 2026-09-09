CREATE TABLE IF NOT EXISTS user_accounts (
	account VARCHAR(191) NOT NULL,
	password VARCHAR(255) NULL COMMENT 'Argon2id PHC; NULL means password login disabled for this account',
	simple_players MEDIUMBLOB,
	PRIMARY KEY (account)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='user_accounts';

CREATE TABLE IF NOT EXISTS account_share_database (
  account MEDIUMTEXT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='account_share_database';

CREATE TABLE IF NOT EXISTS player_centre_database (
  player_id bigint unsigned NOT NULL DEFAULT 0,
  scene_info MEDIUMBLOB
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='player_centre_database';

CREATE TABLE IF NOT EXISTS player_database (
  player_id bigint unsigned NOT NULL DEFAULT 0,
  transform MEDIUMBLOB,
  uint64_pb_component MEDIUMBLOB,
  skill_list MEDIUMBLOB,
  uint32_pb_component MEDIUMBLOB,
  derived_attributes_component MEDIUMBLOB,
  level_component MEDIUMBLOB,
  currency MEDIUMBLOB,
  stress_test_probe MEDIUMBLOB,
  merge_state MEDIUMBLOB,
  attribute_component MEDIUMBLOB,
  pet_component MEDIUMBLOB
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='player_database';

CREATE TABLE IF NOT EXISTS player_database_1 (
  player_id bigint unsigned NOT NULL DEFAULT 0,
  stress_test_probe MEDIUMBLOB
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='player_database_1';
