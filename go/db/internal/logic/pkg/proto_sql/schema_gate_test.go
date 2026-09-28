package proto_sql

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"db/internal/dbtest"
	"db/internal/migrate"

	dbpb "proto/common/database"
	dbopts "proto/db"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// schemaGateHandler 伪造启动期核对的只读查询:information_schema 的 COLUMNS / STATISTICS,
// 以及台账 schema_migrations 里的干净基线计数(baselineCount)。
// columnsErr 非 nil 时 COLUMNS 查询直接失败,模拟库不可达或无权限。
func schemaGateHandler(columns, primaries [][]driver.Value, baselineCount int64, columnsErr error) dbtest.Handler {
	return func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "information_schema.COLUMNS"):
			if columnsErr != nil {
				return nil, columnsErr
			}
			return &dbtest.Rows{Columns: []string{"TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE"}, Values: columns}, nil
		case strings.Contains(query, "information_schema.STATISTICS"):
			return &dbtest.Rows{Columns: []string{"TABLE_NAME"}, Values: primaries}, nil
		case strings.Contains(query, migrate.SchemaMigrationsTable):
			return &dbtest.Rows{Columns: []string{"COUNT(*)"}, Values: [][]driver.Value{{baselineCount}}}, nil
		}
		return nil, nil
	}
}

func schemaGateModel(tables ...proto.Message) *proto2mysql.DB {
	model := proto2mysql.NewDB()
	for _, t := range tables {
		model.RegisterTable(t)
	}
	return model
}

// declaredColumnRows 按 proto 字段逐个造一行 information_schema.COLUMNS,跳过 skip 列。
// 类型统一填 columnType:这里只关心「列在不在」,类型对不对由 Drift 自己的单测覆盖,
// 这样 proto2mysql 的类型映射变化不会让本测试误红。
func declaredColumnRows(msg proto.Message, columnType, skip string) [][]driver.Value {
	table := proto2mysql.GetTableName(msg)
	fields := msg.ProtoReflect().Descriptor().Fields()
	rows := make([][]driver.Value, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if name == skip {
			continue
		}
		rows = append(rows, []driver.Value{table, name, columnType})
	}
	return rows
}

// ledgerColumnRows 让台账表 schema_migrations 出现在 information_schema.COLUMNS 里。
func ledgerColumnRows() [][]driver.Value {
	return [][]driver.Value{{migrate.SchemaMigrationsTable, "version", "bigint unsigned"}}
}

// assertReadOnly 钉住「启动期核对对库零写入」:只允许 SELECT,不许 ALTER / SET SESSION / GET_LOCK。
func assertReadOnly(t *testing.T, rec *dbtest.Recorder) {
	t.Helper()
	statements := rec.Statements()
	if len(statements) == 0 {
		t.Fatal("核对应至少查一次 information_schema")
	}
	for _, s := range statements {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SELECT") {
			t.Fatalf("启动期 schema 核对必须只读,却下发了: %s", s)
		}
	}
}

// 2026-09-22 事故的回归:库里缺 proto 声明的列时 InitDB 必须拒启,并在报错里点名「表.列」与补救命令。
func TestSchemaGateRejectsMissingColumn(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	table := proto2mysql.GetTableName(msg)
	db, rec := dbtest.Open(t, schemaGateHandler(
		declaredColumnRows(msg, "bigint unsigned", "data"),
		[][]driver.Value{{table}},
		0,
		nil,
	))

	err := assertSchemaUpToDate(context.Background(), db, schemaGateModel(msg), "zone_1_db", []proto.Message{msg})
	if err == nil {
		t.Fatal("缺列时必须拒启")
	}
	if !strings.Contains(err.Error(), table+".data") {
		t.Fatalf("报错必须点名缺的「表.列」,got: %v", err)
	}
	if strings.Contains(err.Error(), table+".id") {
		t.Fatalf("已存在的列不应出现在缺列清单里,got: %v", err)
	}
	if !strings.Contains(err.Error(), migrateRemedyCommand) {
		t.Fatalf("报错必须带补救命令 %q,got: %v", migrateRemedyCommand, err)
	}
	assertReadOnly(t, rec)
}

// 缺表且 up 会跑基线时必须拒启。zone 库由 deploy/mysql-init 预建成空库、启动器与 K8s 都不跑迁移,
// 若只把「查不到表」当告警,进程照常起、每条 db_task 以 Error 1146 进死信 —— 正是这道闸要防的静默故障。
func TestSchemaGateRejectsMissingTableWithoutCleanBaseline(t *testing.T) {
	cases := []struct {
		name          string
		columns       [][]driver.Value
		baselineCount int64
		wantLedgerHit bool
	}{
		// 空库:连台账都没有,不应去查一张不存在的台账表。
		{name: "空库", columns: nil, baselineCount: 0, wantLedgerHit: false},
		// 台账在但没有干净基线(建表跑到一半留了 dirty,或基线行不存在):up 仍会跑基线。
		{name: "台账无干净基线", columns: ledgerColumnRows(), baselineCount: 0, wantLedgerHit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &dbpb.PlayerSnapshot{}
			table := proto2mysql.GetTableName(msg)
			db, rec := dbtest.Open(t, schemaGateHandler(tc.columns, nil, tc.baselineCount, nil))

			err := assertSchemaUpToDate(context.Background(), db, schemaGateModel(msg), "zone_1_db", []proto.Message{msg})
			if err == nil {
				t.Fatal("缺表且基线未干净应用时必须拒启")
			}
			if !strings.Contains(err.Error(), table) {
				t.Fatalf("报错必须点名缺的表 %s,got: %v", table, err)
			}
			if !strings.Contains(err.Error(), migrateRemedyCommand) {
				t.Fatalf("报错必须带补救命令 %q,got: %v", migrateRemedyCommand, err)
			}
			if got := rec.Contains(migrate.SchemaMigrationsTable); got != tc.wantLedgerHit {
				t.Fatalf("是否查台账: got %v, want %v", got, tc.wantLedgerHit)
			}
			assertReadOnly(t, rec)
		})
	}
}

// 基线已干净应用时 up 不会重建缺的表,拦下来给不出能执行的补救命令 —— 只记日志,不阻断。
func TestSchemaGateOnlyLogsMissingTableAfterCleanBaseline(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	db, rec := dbtest.Open(t, schemaGateHandler(ledgerColumnRows(), nil, 1, nil))

	if err := assertSchemaUpToDate(context.Background(), db, schemaGateModel(msg), "zone_1_db", []proto.Message{msg}); err != nil {
		t.Fatalf("基线已应用时缺表只应告警,got: %v", err)
	}
	if !rec.Contains(migrate.SchemaMigrationsTable) {
		t.Fatal("台账表存在时必须查干净基线再决定是否拒启")
	}
	assertReadOnly(t, rec)
}

// 类型漂移 / 多余列 / 缺主键只记日志不阻断:本地库有 user_oauth.provider_id、user_phone.phone
// 这类已知历史类型漂移,若也拦下,不带 -allow-modify 就永远起不来。
func TestSchemaGateOnlyLogsNonAdditiveDrift(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	table := proto2mysql.GetTableName(msg)
	// geometry 与任何 proto 映射出的类型都不兼容 → 每一列都是类型漂移。
	columns := append(declaredColumnRows(msg, "geometry", ""), []driver.Value{table, "legacy_column", "mediumblob"})
	db, rec := dbtest.Open(t, schemaGateHandler(
		columns,
		nil, // 缺主键
		0,
		nil,
	))

	if err := assertSchemaUpToDate(context.Background(), db, schemaGateModel(msg), "zone_1_db", []proto.Message{msg}); err != nil {
		t.Fatalf("没有缺列时不应拒启,got: %v", err)
	}
	assertReadOnly(t, rec)
}

// 核对本身做不成(库不可达 / 无权限)时 fail-closed,与 friend / trade 的 ensureSchema 同口径。
func TestSchemaGateFailsClosedWhenSchemaUnreadable(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	boom := errors.New("information_schema unavailable")
	db, _ := dbtest.Open(t, schemaGateHandler(nil, nil, 0, boom))

	err := assertSchemaUpToDate(context.Background(), db, schemaGateModel(msg), "zone_1_db", []proto.Message{msg})
	if err == nil {
		t.Fatal("核对失败时必须拒启")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("必须保留底层错误链,got: %v", err)
	}
	if !strings.Contains(err.Error(), migrateRemedyCommand) {
		t.Fatalf("报错必须带补救命令 %q,got: %v", migrateRemedyCommand, err)
	}
}

// packagedTableMessage 造一张「proto 写了 package」的表:full name = gatetest.gate_packaged_table,
// OptionTableName = gate_packaged_table。用 dynamicpb 现拼,不为测试新增 .proto。
func packagedTableMessage(t *testing.T) proto.Message {
	t.Helper()
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, dbopts.E_OptionTableName, "gate_packaged_table")
	// 构造方式与 db_schema_contract_test.go 的 TestBinaryPrimaryKeyUsesBoundedFullColumn 一致。
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("schema_gate_packaged_table.proto"),
		Package: proto.String("gatetest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:    proto.String("gate_packaged_table"),
			Options: opts,
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build packaged table descriptor: %v", err)
	}
	return dynamicpb.NewMessage(fd.Messages().Get(0))
}

// Drift 按 proto full name 查 information_schema;表 proto 一旦加了 package,full name 就不再等于
// 物理表名,每张表都成了「查不到」,闸门会对全部表静默放行。必须在查库之前就拒启。
func TestSchemaGateRejectsFullNameThatIsNotPhysicalTableName(t *testing.T) {
	msg := packagedTableMessage(t)
	if proto2mysql.GetTableName(msg) == "gate_packaged_table" {
		t.Fatal("测试前提失效:带 package 的消息 full name 不应等于物理表名")
	}
	db, rec := dbtest.Open(t, schemaGateHandler(nil, nil, 0, nil))

	err := assertSchemaUpToDate(context.Background(), db, proto2mysql.NewDB(), "zone_1_db", []proto.Message{msg})
	if err == nil {
		t.Fatal("full name 与 OptionTableName 不一致时必须拒启")
	}
	if !strings.Contains(err.Error(), "gate_packaged_table") {
		t.Fatalf("报错必须点名表,got: %v", err)
	}
	if n := len(rec.Statements()); n != 0 {
		t.Fatalf("表名守卫必须在查库之前拦下,却下发了 %d 条语句", n)
	}
}
