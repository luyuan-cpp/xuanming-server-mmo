-- ⚠️ 本文件是历史手工导出,**不是权威 DDL**。
--
-- 权威建表走 db 服务启动时的 proto2mysql.CreateOrUpdateTable(键/列都来自
-- proto/common/database/mysql_database_table.proto 的 OptionPrimaryKey 等选项)。
-- 用本文件预建过表的环境有一个坑:运行时是 CREATE TABLE IF NOT EXISTS,
-- 表已存在就静默跳过,而 UpdateTableField 只补列、**从不补主键** —— 按旧版
-- 本文件(无主键)建出的表会永久缺主键,INSERT ... ON DUPLICATE KEY UPDATE
-- 的幂等语义整个失效(每次存盘追加新行)。
-- 已按 proto 声明补齐主键;存量环境请用 `SHOW KEYS FROM player_database`
-- 核对,缺失则手工 `ALTER TABLE ... ADD PRIMARY KEY (player_id)`。

CREATE TABLE IF NOT EXISTS user_accounts (
	account VARCHAR(191) NOT NULL,
	password VARCHAR(255) NULL COMMENT 'Argon2id PHC; NULL means password login disabled for this account',
	simple_players MEDIUMBLOB,
	PRIMARY KEY (account)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='user_accounts';

-- ⚠️ proto 声明 PK=account,但本文件老版本把 account 写成 MEDIUMTEXT(TEXT 列
-- 不能做无前缀主键)。运行时 DDL 按 proto string 渲染为 VARCHAR;此处不手工
-- 猜测列型,新环境请让 db 服务自行建表,存量环境按运行时 DDL 对齐。
CREATE TABLE IF NOT EXISTS account_share_database (
  account MEDIUMTEXT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='account_share_database';

CREATE TABLE IF NOT EXISTS player_centre_database (
  player_id bigint unsigned NOT NULL DEFAULT 0,
  scene_info MEDIUMBLOB,
  PRIMARY KEY (player_id)
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
  pet_component MEDIUMBLOB,
  PRIMARY KEY (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='player_database';

CREATE TABLE IF NOT EXISTS player_database_1 (
  player_id bigint unsigned NOT NULL DEFAULT 0,
  stress_test_probe MEDIUMBLOB,
  PRIMARY KEY (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='player_database_1';
