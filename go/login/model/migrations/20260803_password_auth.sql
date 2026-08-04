-- 生产口令认证 schema 门禁（MySQL 8.x）。
-- 必须在维护窗口、备份完成后执行。任何 NULL/空/重复/过长 account 都会
-- SIGNAL 并在 ALTER 前停止；脚本绝不截断或猜测如何合并账号。
-- password 旧明文不会被自动转换：ALTER 后仍原样保留，但 Login 只接受
-- Argon2id PHC，因此这些账号在 password_admin 完成迁移前保持 fail-closed。

DELIMITER $$
DROP PROCEDURE IF EXISTS migrate_login_password_auth$$
CREATE PROCEDURE migrate_login_password_auth()
BEGIN
    DECLARE bad_count BIGINT DEFAULT 0;
    DECLARE primary_count BIGINT DEFAULT 0;
    DECLARE account_primary_count BIGINT DEFAULT 0;
	DECLARE primary_column_count BIGINT DEFAULT 0;

    SELECT COUNT(*) INTO bad_count
      FROM user_accounts
     WHERE account IS NULL OR account = '' OR CHAR_LENGTH(account) > 191;
    IF bad_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'user_accounts has NULL/empty/>191-char account; refusing password auth migration';
    END IF;

	SELECT COUNT(*) INTO bad_count
	  FROM user_accounts
	 WHERE password IS NOT NULL AND CHAR_LENGTH(password) > 255;
	IF bad_count <> 0 THEN
		SIGNAL SQLSTATE '45000'
			SET MESSAGE_TEXT = 'user_accounts has >255-char password data; refusing truncating migration';
	END IF;

    SELECT COUNT(*) INTO bad_count
      FROM (
          SELECT account
            FROM user_accounts
           GROUP BY account
          HAVING COUNT(*) > 1
      ) duplicate_accounts;
    IF bad_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'user_accounts has duplicate account values; refusing password auth migration';
    END IF;

    SELECT COUNT(*) INTO primary_count
      FROM information_schema.TABLE_CONSTRAINTS
     WHERE CONSTRAINT_SCHEMA = DATABASE()
       AND TABLE_NAME = 'user_accounts'
       AND CONSTRAINT_TYPE = 'PRIMARY KEY';

    SELECT COUNT(*) INTO account_primary_count
      FROM information_schema.KEY_COLUMN_USAGE
     WHERE CONSTRAINT_SCHEMA = DATABASE()
       AND TABLE_NAME = 'user_accounts'
       AND CONSTRAINT_NAME = 'PRIMARY'
       AND COLUMN_NAME = 'account'
       AND ORDINAL_POSITION = 1;

	SELECT COUNT(*) INTO primary_column_count
	  FROM information_schema.KEY_COLUMN_USAGE
	 WHERE CONSTRAINT_SCHEMA = DATABASE()
	   AND TABLE_NAME = 'user_accounts'
	   AND CONSTRAINT_NAME = 'PRIMARY';

	IF primary_count <> 0 AND (account_primary_count <> 1 OR primary_column_count <> 1) THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'user_accounts has a non-account primary key; refusing automatic replacement';
    END IF;

    ALTER TABLE user_accounts
        MODIFY COLUMN account VARCHAR(191) NOT NULL,
        MODIFY COLUMN password VARCHAR(255) NULL
            COMMENT 'Argon2id PHC; NULL means password login disabled for this account';

    IF primary_count = 0 THEN
        ALTER TABLE user_accounts ADD PRIMARY KEY (account);
    END IF;
END$$
CALL migrate_login_password_auth()$$
DROP PROCEDURE migrate_login_password_auth$$
DELIMITER ;
