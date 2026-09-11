package main

import (
	"reflect"
	"testing"

	"db/internal/logic/pkg/proto_sql"

	"github.com/go-sql-driver/mysql"
)

func TestMigrationConfigCanReadLedgerTimesWithoutChangingSharedSettings(t *testing.T) {
	shared := proto_sql.NewMysqlConfig()
	migration := migrationMysqlConfig()
	parsed, err := mysql.ParseDSN(migration.FormatDSN())
	if err != nil {
		t.Fatalf("parse migration connection options: %v", err)
	}
	if !parsed.ParseTime {
		t.Fatal("migration connection must decode ledger DATETIME fields for time.Time and sql.NullTime scans")
	}

	// 时间解析以外的参数必须仍与业务服务一致,包括数据库目标和严格模式。
	migration.ParseTime = shared.ParseTime
	if !reflect.DeepEqual(migration, shared) {
		t.Fatal("migration changed shared connection settings beyond ledger time decoding")
	}
}
