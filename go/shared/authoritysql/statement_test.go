package authoritysql

import "testing"

func TestOnlySchemaAcceptsExistingMigrationControl(t *testing.T) {
	for _, query := range []string{
		"SET SESSION lock_wait_timeout = 5, innodb_lock_wait_timeout = 5",
		"KILL QUERY 42",
		"SET DEFAULT ROLE 'fixture_role'@'%' TO 'fixture_writer'@'%'",
		"CREATE TABLE t(id INT PRIMARY KEY /*T![clustered_index] NONCLUSTERED */) /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */",
	} {
		if err := validatePermissionStatement(query, permissionSchema); err != nil {
			t.Fatalf("正式schema语句被拒: %v", err)
		}
		for _, kind := range []permissionKind{permissionReader, permissionWriter, permissionOperator} {
			if validatePermissionStatement(query, kind) == nil {
				t.Fatal("非Schema能力越界")
			}
		}
	}
	for _, query := range []string{"SET autocommit=1", "SET SESSION tidb_snapshot='old'", "KILL CONNECTION 42", "KILL QUERY 42; COMMIT", "SET SESSION lock_wait_timeout=5, innodb_lock_wait_timeout=5; UPDATE t SET n=1", "CREATE TABLE t(id INT) /*T! ; DROP TABLE x */", "SELECT 1 /*T![clustered_index] CLUSTERED */"} {
		if validatePermissionStatement(query, permissionSchema) == nil {
			t.Fatal("Schema接受了非授权控制语句")
		}
	}
}

// 隔离候选，尚未运行；这些输入是未来正式红绿验收的接口面。
func TestRuntimeStatementEnvelope(t *testing.T) {
	cases := []struct {
		name, sql string
		allowed   bool
	}{
		{"简单查询", "SELECT account FROM user_accounts WHERE account=?", true},
		{"参数更新", "UPDATE wallet SET amount=amount+? WHERE id=?", true},
		{"插入冲突", "INSERT INTO t(id) VALUES(?) ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)", true},
		{"替换", "REPLACE INTO t(id) VALUES(?)", true},
		{"公共表表达式", "WITH x AS (SELECT id FROM t) SELECT * FROM x", true},
		{"锁定读", "SELECT id FROM t WHERE id=? FOR UPDATE", true},
		{"解释", "EXPLAIN ANALYZE SELECT id FROM t", true},
		{"中文值", "SELECT '中文;/*文本*/'", true},
		{"分号值", "SELECT ';COMMIT;' ; -- 只有尾注释", true},
		{"反引号转义", "SELECT `a``b` FROM t", true},
		{"单引号转义", "SELECT 'a'';COMMIT'", true},
		{"反斜线转义", "SELECT 'a\\';COMMIT'", true},
		{"普通注释", "/* 说明 */ SELECT 1; # 尾说明", true},
		{"好友TiDB提示", "SELECT /*+ MAX_EXECUTION_TIME(1000) INL_JOIN(f@qb_f) */ id FROM t", true},
		{"好友MySQL提示", "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ id FROM t", true},
		{"查询块提示", "SELECT /*+ QB_NAME(qb_fof) */ id FROM t", true},
		{"空白", "\t\r\n", false},
		{"只有注释", "/* 空 */", false},
		{"重复尾分号", "SELECT 1;;", false},
		{"多语句提交", "UPDATE t SET n=1; COMMIT", false},
		{"多语句会话", "SELECT 1;SET autocommit=1", false},
		{"事务开始", "BEGIN", false},
		{"显式开始", "START TRANSACTION", false},
		{"事务提交", "COMMIT", false},
		{"事务回滚", "ROLLBACK", false},
		{"保存点", "SAVEPOINT x", false},
		{"会话设置", "SET autocommit=1", false},
		{"选库", "USE other_schema", false},
		{"DDL", "CREATE TABLE t(id int)", false},
		{"存储过程", "CALL p()", false},
		{"动态预处理", "PREPARE s FROM @sql", false},
		{"版本注释", "SELECT 1 /*! ; COMMIT */", false},
		{"TiDB注释", "SELECT 1 /*T! ; COMMIT */", false},
		{"MariaDB注释", "SELECT 1 /*M! ; COMMIT */", false},
		{"嵌套注释", "/* a /* b */ SELECT 1", false},
		{"未闭注释", "SELECT 1 /*", false},
		{"未闭字符串", "SELECT 'abc", false},
		{"前缀假匹配", "SELECTFORUPDATE 1", false},
		{"提示改变量", "SELECT /*+ SET_VAR(autocommit=1) */ 1", false},
		{"提示读副本", "SELECT /*+ READ_FROM_STORAGE(TIFLASH[t]) */ * FROM t", false},
		{"未知提示", "SELECT /*+ SOMETHING(x) */ 1", false},
		{"提示内注入", "SELECT /*+ QB_NAME(x); COMMIT */ 1", false},
		{"不明编码", "SELECT \x00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimeStatement(tc.sql)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, err=%v", tc.allowed, err)
			}
		})
	}
}

func TestSchemaSplitUsesOnlyExistingNumericBoundaryGrammar(t *testing.T) {
	allowed := []string{"SPLIT TABLE `db`.`player_route` BY (1), (1000)", "SPLIT TABLE `player_route` INDEX `idx_route_updated` BY (1, 5), (2, 10)"}
	for _, query := range allowed {
		if err := validatePermissionStatement(query, permissionSchema); err != nil {
			t.Fatalf("已审预切形状被拒: %v", err)
		}
		for _, kind := range []permissionKind{permissionWriter, permissionReader, permissionOperator} {
			if validatePermissionStatement(query, kind) == nil {
				t.Fatal("非Schema可预切Region")
			}
		}
	}
	for _, query := range []string{"SPLIT TABLE `t` BY (1); DELETE FROM `t`", "SPLIT TABLE `t` BY ((SELECT 1))", "SPLIT TABLE `t` BY (1+2)", "SPLIT TABLE t BY (1)", "SPLIT TABLE `t` BY (1) /*x*/", "SPLIT TABLE `t` BETWEEN (1) AND (10) REGIONS 4"} {
		if validatePermissionStatement(query, permissionSchema) == nil {
			t.Fatalf("额外预切语法被放行: %s", query)
		}
	}
}
