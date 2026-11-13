-- 口令凭据表(login 的 PasswordAuth)。归属:mmorpg 逻辑库,全服一份;login 是唯一读者,写入只走 cmd/password_admin。
-- 本文件是这张表建表语句的唯一来源:
--   - K8s:mmorpg 的建表 Job(gateway-schema)用 writer_authority prepare-schema -ddl-file 执行。所以这里只能有
--     CREATE TABLE IF NOT EXISTS,表名不带库名,注释整行写,语句(含 COMMENT 字符串)里不出现分号。
--   - 本机与集群内开发 MySQL:deploy/mysql-init/gateway_tables.sql 里有逐字相同的一条,契约测试钉住两边一致。
-- 表的形状由 login 与 password_admin 启动时核对(internal/logic/pkg/auth/password_schema.go),改这里要同步改那边的守卫与测试。
-- 排序规则显式写:账号匹配不区分大小写与重音。只写 CHARSET 时 MySQL 与 TiDB 取的缺省排序规则不同,匹配语义会不一样。
-- 存档库里同名的 user_accounts 是 db 服务的旧表(带 simple_players),与这张表无关,不是口令权威。

CREATE TABLE IF NOT EXISTS user_accounts (
    account  VARCHAR(191) NOT NULL,
    password VARCHAR(255) NULL COMMENT 'Argon2id PHC string. NULL = password login disabled for this account',
    PRIMARY KEY (account)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='login password credentials, owner go/login';
