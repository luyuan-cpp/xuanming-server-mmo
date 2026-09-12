package migrate

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"db/internal/dbtest"

	dbpb "proto/common/database"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
)

func newTestSource(t *testing.T, allowModify bool) *ProtoSource {
	t.Helper()
	model := proto2mysql.NewDB()
	tables := []proto.Message{&dbpb.PlayerDatabase{}, &dbpb.PlayerSnapshot{}}
	for _, tbl := range tables {
		model.RegisterTable(tbl)
	}
	return &ProtoSource{Model: model, Tables: tables, AllowModifyColumn: allowModify}
}

func TestBaselineIsCreateIfNotExistsOnly(t *testing.T) {
	src := newTestSource(t, false)
	m, err := src.Baseline()
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if m.Version != BaselineVersion || m.Name != BaselineName {
		t.Fatalf("baseline identity = %d/%s", m.Version, m.Name)
	}
	if len(m.Statements) != 2 {
		t.Fatalf("expected one statement per table, got %d", len(m.Statements))
	}
	for _, s := range m.Statements {
		// 基线必须是幂等的:对已有库重复跑不能报错、更不能改动已有表。
		if !strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS") {
			t.Fatalf("baseline statement is not idempotent: %s", s)
		}
	}
}

func TestBaselineChecksumTracksTableSetNotDDLText(t *testing.T) {
	// 基线 checksum 只算表名集合:proto 加一个字段就会改变建表语句文本,
	// 如果 checksum 跟着变,每次改 proto 都会被当成「基线变了」而误报。
	// 列级变更由漂移迁移和它自己的 checksum 负责。
	a, err := newTestSource(t, false).Baseline()
	if err != nil {
		t.Fatalf("baseline a: %v", err)
	}

	model := proto2mysql.NewDB()
	// 换个顺序注册同一组表,checksum 必须不变。
	tables := []proto.Message{&dbpb.PlayerSnapshot{}, &dbpb.PlayerDatabase{}}
	for _, tbl := range tables {
		model.RegisterTable(tbl)
	}
	b, err := (&ProtoSource{Model: model, Tables: tables}).Baseline()
	if err != nil {
		t.Fatalf("baseline b: %v", err)
	}
	if a.Checksum != b.Checksum {
		t.Fatalf("table-set checksum must be order independent: %s vs %s", a.Checksum, b.Checksum)
	}

	model2 := proto2mysql.NewDB()
	tables2 := []proto.Message{&dbpb.PlayerDatabase{}}
	model2.RegisterTable(tables2[0])
	c, err := (&ProtoSource{Model: model2, Tables: tables2}).Baseline()
	if err != nil {
		t.Fatalf("baseline c: %v", err)
	}
	if a.Checksum == c.Checksum {
		t.Fatal("removing a table must change the baseline checksum")
	}
}

func TestTypesCompatible(t *testing.T) {
	cases := []struct {
		current string
		target  string
		want    bool
	}{
		{"bigint unsigned", "bigint unsigned NOT NULL DEFAULT 0", true},
		{"bigint unsigned", "bigint unsigned NOT NULL AUTO_INCREMENT", true},
		{"bigint", "bigint unsigned NOT NULL DEFAULT 0", false}, // 少了 unsigned 会截半个值域
		{"mediumblob", "MEDIUMBLOB", true},
		{"tinyint(1)", "tinyint(1) NOT NULL DEFAULT 0", true},
		{"int unsigned", "int unsigned NOT NULL DEFAULT 0", true},
		{"varchar(191)", "varchar(255) NOT NULL", true}, // 只比类型族,不比长度
		{"varchar(191)", "MEDIUMTEXT", false},           // 换族才是危险的(主键做不了)
		{"mediumtext", "MEDIUMBLOB", false},
		{"double", "double NOT NULL DEFAULT 0", true},
	}
	for _, tc := range cases {
		if got := TypesCompatible(tc.current, tc.target); got != tc.want {
			t.Errorf("TypesCompatible(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

// driftHandler 伪造 information_schema 的两次查询。
func driftHandler(columns [][]driver.Value, primaries [][]driver.Value) dbtest.Handler {
	return func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "information_schema.COLUMNS"):
			return &dbtest.Rows{Columns: []string{"TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE"}, Values: columns}, nil
		case strings.Contains(query, "information_schema.STATISTICS"):
			return &dbtest.Rows{Columns: []string{"TABLE_NAME"}, Values: primaries}, nil
		}
		return nil, nil
	}
}

func TestDriftAddsMissingColumns(t *testing.T) {
	src := newTestSource(t, false)
	// player_database 只建了 player_id,其余 blob 列全缺。
	db, _ := dbtest.Open(t, driftHandler(
		[][]driver.Value{
			{"player_database", "player_id", "bigint unsigned"},
			{"player_snapshot", "id", "bigint unsigned"},
			{"player_snapshot", "player_id", "bigint unsigned"},
			{"player_snapshot", "zone_id", "int unsigned"},
			{"player_snapshot", "snapshot_type", "int unsigned"},
			{"player_snapshot", "created_at", "bigint unsigned"},
			{"player_snapshot", "reason", "mediumtext"},
			{"player_snapshot", "operator", "mediumtext"},
			{"player_snapshot", "data", "mediumblob"},
		},
		[][]driver.Value{{"player_database"}, {"player_snapshot"}},
	))

	m, warnings, err := src.Drift(context.Background(), db, "zone_1_db")
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if len(m.Statements) == 0 {
		t.Fatal("missing columns must produce ADD COLUMN statements")
	}
	for _, s := range m.Statements {
		if !strings.Contains(s, "ADD COLUMN") {
			t.Fatalf("drift must only emit ADD COLUMN by default, got: %s", s)
		}
	}
	if !strings.Contains(strings.Join(m.Statements, "\n"), "`transform`") {
		t.Fatalf("expected transform to be added, got %v", m.Statements)
	}
	if m.Version != 0 {
		t.Fatalf("drift version must be allocated by the runner, got %d", m.Version)
	}
	if m.Checksum == "" || !strings.HasPrefix(m.Name, "auto_proto_sync_") {
		t.Fatalf("drift identity must come from its checksum, got name=%s checksum=%s", m.Name, m.Checksum)
	}
	if len(warnings) != 0 {
		t.Fatalf("pure additions must not need review: %v", warnings)
	}
}

func TestDriftReportsTypeMismatchInsteadOfModifying(t *testing.T) {
	// 这是本次治理的关键行为差异:proto2mysql 的 UpdateTableField 在类型不匹配时
	// 会直接拼 MODIFY COLUMN 并 Exec —— 大表上是重建表级别的在线 DDL,而且
	// 对主键列会直接失败。默认改为只报告。
	src := newTestSource(t, false)
	db, _ := dbtest.Open(t, driftHandler(
		[][]driver.Value{
			{"player_database", "player_id", "varchar(64)"},
			{"player_snapshot", "id", "bigint unsigned"},
		},
		[][]driver.Value{{"player_database"}, {"player_snapshot"}},
	))

	m, warnings, err := src.Drift(context.Background(), db, "zone_1_db")
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	for _, s := range m.Statements {
		if strings.Contains(s, "MODIFY COLUMN") {
			t.Fatalf("type drift must not be auto-applied without -allow-modify: %s", s)
		}
	}
	var found bool
	for _, w := range warnings {
		if strings.Contains(w, "列类型漂移") && strings.Contains(w, "player_database.player_id") {
			found = true
		}
	}
	if !found {
		t.Fatalf("type drift must be reported for review, got %v", warnings)
	}

	// 显式授权后才生成 MODIFY COLUMN。
	srcModify := newTestSource(t, true)
	db2, _ := dbtest.Open(t, driftHandler(
		[][]driver.Value{
			{"player_database", "player_id", "varchar(64)"},
			{"player_snapshot", "id", "bigint unsigned"},
		},
		[][]driver.Value{{"player_database"}, {"player_snapshot"}},
	))
	m2, _, err := srcModify.Drift(context.Background(), db2, "zone_1_db")
	if err != nil {
		t.Fatalf("drift with -allow-modify: %v", err)
	}
	if !strings.Contains(strings.Join(m2.Statements, "\n"), "MODIFY COLUMN `player_id`") {
		t.Fatalf("-allow-modify must emit MODIFY COLUMN, got %v", m2.Statements)
	}
}

func TestDriftReportsMissingPrimaryKeyAndExtraColumns(t *testing.T) {
	// 缺主键会让 INSERT ... ON DUPLICATE KEY UPDATE 的幂等语义整个失效
	// (每次存盘追加新行),但补主键在有重复行时会失败,只能人工处理。
	src := newTestSource(t, false)
	db, _ := dbtest.Open(t, driftHandler(
		[][]driver.Value{
			{"player_database", "player_id", "bigint unsigned"},
			{"player_database", "legacy_column", "mediumblob"},
			{"player_snapshot", "id", "bigint unsigned"},
		},
		// player_database 没有主键
		[][]driver.Value{{"player_snapshot"}},
	))

	_, warnings, err := src.Drift(context.Background(), db, "zone_1_db")
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "缺主键") || !strings.Contains(joined, "player_database") {
		t.Fatalf("missing primary key must be reported, got %v", warnings)
	}
	if !strings.Contains(joined, "多余列") || !strings.Contains(joined, "legacy_column") {
		t.Fatalf("extra columns must be reported (and never dropped), got %v", warnings)
	}
}

func TestAdvisoryLockNameIsPerDatabase(t *testing.T) {
	// 不同 zone 是不同的库、不同的表,必须能并行迁移;同一个库必须串行。
	if advisoryLockName("zone_1_db") == advisoryLockName("zone_2_db") {
		t.Fatal("different databases must not share a migration lock")
	}
	long := advisoryLockName(strings.Repeat("x", 200))
	if len(long) > 64 {
		t.Fatalf("MySQL GET_LOCK names cap at 64 bytes, got %d", len(long))
	}
}

// Baseline 建出来的表必须带主键。
//
// 这条不是形式检查:proto2mysql v0.0.18 的 GetCreateTableSQL 对 player_database
// 不输出 PRIMARY KEY,而 go/db/model/mysql_database_table.sql 里它是
// PRIMARY KEY (player_id)。若原样拿生成的 DDL 建表,玩家数据表会没有主键,
// 而存盘走 INSERT ... ON DUPLICATE KEY UPDATE —— 没有唯一键就永远不命中
// "duplicate",每次存盘追加一行新记录,表无限膨胀且读回来的是任意一行。
func TestBaselineAlwaysEmitsDeclaredPrimaryKey(t *testing.T) {
	src := newTestSource(t, false)
	m, err := src.Baseline()
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for _, stmt := range m.Statements {
		if !strings.Contains(stmt, "PRIMARY KEY") {
			t.Fatalf("建表语句缺主键,会让存盘的 ON DUPLICATE KEY UPDATE 失效:\n%s", stmt)
		}
	}
	// player_database 只标了 OptionIsPlayerDatabase、没标 OptionPrimaryKey,
	// 主键要靠玩家分表约定解出来,单独钉一次。
	var playerDB string
	for _, stmt := range m.Statements {
		if strings.Contains(stmt, "player_database") {
			playerDB = stmt
		}
	}
	if playerDB == "" {
		t.Fatal("baseline 里没有 player_database 建表语句")
	}
	if !strings.Contains(playerDB, "PRIMARY KEY (`player_id`)") {
		t.Fatalf("player_database 主键必须是 player_id,实际:\n%s", playerDB)
	}
}
