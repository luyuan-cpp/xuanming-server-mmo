package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

func TestCurrentTiDBDSNReplacesHistoricalPoliciesIncludingCaseAliases(t *testing.T) {
	for name, build := range map[string]func(string) (string, error){
		"业务池": func(raw string) (string, error) { return readCommittedDSN(raw, authoritysql.BackendTiDB) },
		"路由池": func(raw string) (string, error) { return routeDBDSN(raw, authoritysql.BackendTiDB) },
	} {
		t.Run(name, func(t *testing.T) {
			input := "fixture@tcp(127.0.0.1:4000)/fixture?parseTime=true&tidb_snapshot=%27123%27&TIDB_SNAPSHOT=%27456%27&tidb_read_staleness=-60&tidb_low_resolution_tso=ON&tidb_replica_read=%27follower%27&tidb_read_consistency=%27weak%27&tidb_txn_mode=%27optimistic%27&TX_ISOLATION=%27REPEATABLE-READ%27"
			out, err := build(input)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := mysql.ParseDSN(out)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DBName != "fixture" || cfg.Addr != "127.0.0.1:4000" || !cfg.ParseTime {
				t.Fatal("强读规范化改坏连接目标/编码选项")
			}
			for key, want := range map[string]string{"transaction_isolation": "'READ-COMMITTED'", "tidb_txn_mode": "'pessimistic'", "tidb_replica_read": "'leader'", "tidb_read_consistency": "'strict'", "tidb_snapshot": "''", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF"} {
				if cfg.Params[key] != want {
					t.Fatalf("%s未固定到强读策略", key)
				}
			}
			if _, ok := cfg.Params["TIDB_SNAPSHOT"]; ok {
				t.Fatal("大小写历史快照别名仍存活")
			}
			if _, ok := cfg.Params["TX_ISOLATION"]; ok {
				t.Fatal("大小写旧隔离级别别名仍存活")
			}
			if again, err := build(out); err != nil || again != out {
				t.Fatal("规范化不幂等")
			}
		})
	}
}

// mysql 档:DSN 里任何 tidb_* 都不能留下(驱动会 SET,MySQL 上是 1193),RC 照样固定。
func TestCurrentMySQLDSNDropsEveryTiDBParam(t *testing.T) {
	for name, build := range map[string]func(string) (string, error){
		"业务池": func(raw string) (string, error) { return readCommittedDSN(raw, authoritysql.BackendMySQL) },
		"路由池": func(raw string) (string, error) { return routeDBDSN(raw, authoritysql.BackendMySQL) },
	} {
		t.Run(name, func(t *testing.T) {
			out, err := build("fixture@tcp(127.0.0.1:3306)/fixture?parseTime=true&tidb_snapshot=%27123%27&TIDB_TXN_MODE=%27optimistic%27&tidb_anything_else=1&tx_isolation=%27REPEATABLE-READ%27")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := mysql.ParseDSN(out)
			if err != nil {
				t.Fatal(err)
			}
			for key := range cfg.Params {
				if strings.HasPrefix(strings.ToLower(key), "tidb_") {
					t.Fatalf("mysql 档 DSN 留下了 %s", key)
				}
			}
			if cfg.Params[mysqlIsolationParam] != mysqlReadCommitted {
				t.Fatal("mysql 档也必须固定 RC")
			}
			if _, ok := cfg.Params[mysqlLegacyIsolationParam]; ok {
				t.Fatal("旧隔离级别别名仍存活")
			}
		})
	}
}

// mysql 档读回与服务同一口径:拒 TiDB、拒只读实例、拒非 RC。
func TestCurrentMySQLStartupRefusesWrongServer(t *testing.T) {
	good := []driver.Value{"8.0.36", "MySQL Community Server - GPL", int64(0), int64(0), "READ-COMMITTED"}
	st, _ := newFakeRouteSQLStore(t, routeScript{}.on("MYSQL_SESSION", rsRows(good)))
	if err := requireCurrentSession(context.Background(), st.db, authoritysql.BackendMySQL); err != nil {
		t.Fatalf("主库 + RC 必须通过: %v", err)
	}
	for _, c := range []struct {
		name string
		row  []driver.Value
	}{
		{"TiDB", []driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server", int64(0), int64(0), "READ-COMMITTED"}},
		{"从库", []driver.Value{"8.0.36", "MySQL Community Server - GPL", int64(1), int64(0), "READ-COMMITTED"}},
		{"超级只读", []driver.Value{"8.0.36", "MySQL Community Server - GPL", int64(1), int64(1), "READ-COMMITTED"}},
		{"非RC", []driver.Value{"8.0.36", "MySQL Community Server - GPL", int64(0), int64(0), "REPEATABLE-READ"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, _ := newFakeRouteSQLStore(t, routeScript{}.on("MYSQL_SESSION", rsRows(c.row)))
			if err := requireCurrentSession(context.Background(), st.db, authoritysql.BackendMySQL); !errors.Is(err, errCurrentMySQL) {
				t.Fatalf("必须拒绝: %v", err)
			}
		})
	}
}

// 应答脚本只验证启动失败语义；真正强TiDB会话与被污染DSN的覆盖由merge_integration验收。
func TestCurrentTiDBStartupRefusesUnsafeReadback(t *testing.T) {
	for _, c := range []struct {
		name string
		row  []driver.Value
	}{
		{"历史快照", []driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "123", int64(0), int64(0)}},
		{"副本旧读", []driver.Value{"pessimistic", "READ-COMMITTED", "follower", "strict", "", int64(0), int64(0)}},
		{"时间旧读", []driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(-5), int64(0)}},
		{"低精度TSO", []driver.Value{"pessimistic", "READ-COMMITTED", "leader", "strict", "", int64(0), int64(1)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, _ := newFakeRouteSQLStore(t, routeScript{}.on("VERSION", rsRows([]driver.Value{"8.0.11-TiDB-v8.5.0", "TiDB Server"})).on("TXN_MODE", rsRows(c.row)))
			if err := requireCurrentTiDB(context.Background(), st.db); !errors.Is(err, errCurrentTiDB) {
				t.Fatalf("不安全读回必须拒绝: %v", err)
			}
		})
	}
}
