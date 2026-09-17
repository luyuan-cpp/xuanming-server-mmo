package schemamigrate

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestBuildSchemaDerivesTableFromProtoOptions(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t))
	if len(sch.tables) != 1 {
		t.Fatalf("tables = %d, want 1", len(sch.tables))
	}
	tbl := sch.tables[0]
	if tbl.name != "trade_listing" {
		t.Fatalf("表名必须取 OptionTableName,不取 proto full name,got %q", tbl.name)
	}
	if !strings.HasPrefix(tbl.createSQL, "CREATE TABLE IF NOT EXISTS `trade_listing` (") || strings.HasSuffix(tbl.createSQL, ";") {
		t.Fatalf("建表语句形状不对: %s", tbl.createSQL)
	}
	if !containsAll(tbl.createSQL, "/*T![clustered_index] NONCLUSTERED */", "SHARD_ROW_ID_BITS=4", "PRE_SPLIT_REGIONS=4") {
		t.Fatalf("TiDB 方言选项必须进建表语句: %s", tbl.createSQL)
	}
	if !slices.Equal(tbl.primaryKey, []string{"listing_id"}) {
		t.Fatalf("primaryKey = %v", tbl.primaryKey)
	}
	if !slices.Equal(tbl.indexes, []string{"idx_trade_listing_0", "idx_trade_listing_1"}) {
		t.Fatalf("indexes = %v", tbl.indexes)
	}
	if len(tbl.columns) != 7 {
		t.Fatalf("columns = %d, want 7", len(tbl.columns))
	}
	first := tbl.columns[0]
	if first.name != "listing_id" || !strings.HasPrefix(first.definition, "bigint unsigned") || !strings.Contains(first.definition, "COMMENT 'pb:1'") {
		t.Fatalf("列定义必须原样取 proto2mysql 的输出(含 pb:N 注释),got %+v", first)
	}
}

func TestBuildSchemaAcceptsCompositeIntegerAndEnumPrimaryKey(t *testing.T) {
	tbl := listingTable()
	tbl.pk = "category,listing_id"
	sch := mustSchema(t, tbl.build(t), favoriteTable().build(t))
	if !slices.Equal(sch.tables[0].primaryKey, []string{"category", "listing_id"}) {
		t.Fatalf("primaryKey = %v", sch.tables[0].primaryKey)
	}
	if !slices.Equal(sch.tables[1].primaryKey, []string{"player_id", "listing_id"}) {
		t.Fatalf("primaryKey = %v", sch.tables[1].primaryKey)
	}
}

func TestBuildSchemaRejectsInvalidTableLists(t *testing.T) {
	noName := listingTable()
	noName.table = ""
	dupName := favoriteTable()
	dupName.table = "trade_listing"
	reserved := listingTable()
	reserved.table = SchemaMigrationsTable
	badName := listingTable()
	badName.table = "trade-listing"
	stringPK := listingTable()
	stringPK.pk = "title"
	noPK := listingTable()
	noPK.pk = ""
	unknownPK := listingTable()
	unknownPK.pk = "nope"
	sint := listingTable().withField(testField{"delta", 8, tSint64})

	cases := []struct {
		name    string
		tables  []proto.Message
		wantErr string
	}{
		{"空清单", nil, "不能为空"},
		{"nil 元素", []proto.Message{listingTable().build(t), nil}, "Tables[1] 为 nil"},
		{"缺 OptionTableName", []proto.Message{noName.build(t)}, "未声明 OptionTableName"},
		{"表名重复", []proto.Message{listingTable().build(t), dupName.build(t)}, "重复"},
		{"与台账重名", []proto.Message{reserved.build(t)}, "台账表重名"},
		{"表名含非法字符", []proto.Message{badName.build(t)}, "非法"},
		{"string 主键", []proto.Message{stringPK.build(t)}, "只允许整数或枚举列"},
		{"缺主键", []proto.Message{noPK.build(t)}, "未声明 OptionPrimaryKey"},
		{"主键不是字段", []proto.Message{unknownPK.build(t)}, "不是消息"},
		{"字段类型没有列映射", []proto.Message{sint.build(t)}, "没有 MySQL 列类型映射"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSchema(tc.tables)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want 含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestBaselineIsCreateIfNotExistsOnly(t *testing.T) {
	m := mustSchema(t, listingTable().build(t), favoriteTable().build(t)).baseline()
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
		// 新服务的表一律带主键(D-14),主键缺失在 buildSchema 就被拒。
		if !strings.Contains(s, "PRIMARY KEY (") {
			t.Fatalf("建表语句缺主键: %s", s)
		}
	}
	if !strings.Contains(m.Statements[0], "`trade_listing`") || !strings.Contains(m.Statements[1], "`trade_favorite`") {
		t.Fatalf("基线语句顺序必须与表清单一致: %v", collapseAll(m.Statements))
	}
}

func TestBaselineChecksumTracksTableSetNotDDLText(t *testing.T) {
	// 基线 checksum 只算表名集合:proto 加一个字段就会改变建表语句文本,
	// 如果 checksum 跟着变,每次改 proto 都会被当成「基线变了」。
	a := mustSchema(t, listingTable().build(t), favoriteTable().build(t)).baseline()
	b := mustSchema(t, favoriteTable().build(t), listingTable().build(t)).baseline()
	if a.Checksum != b.Checksum {
		t.Fatalf("table-set checksum must be order independent: %s vs %s", a.Checksum, b.Checksum)
	}
	grown := listingTable().withField(testField{"level", 8, tUint32})
	c := mustSchema(t, grown.build(t), favoriteTable().build(t)).baseline()
	if a.Checksum != c.Checksum {
		t.Fatal("adding a column must not change the baseline checksum")
	}
	d := mustSchema(t, listingTable().build(t)).baseline()
	if a.Checksum == d.Checksum {
		t.Fatal("removing a table must change the baseline checksum")
	}
}

func TestTypesCompatible(t *testing.T) {
	cases := []struct {
		current string
		target  string
		want    bool
	}{
		{"bigint unsigned", "bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:1'", true},
		{"bigint(20) unsigned", "bigint unsigned NOT NULL DEFAULT 0", true}, // TiDB / 旧 MySQL 带显示宽度
		{"bigint", "bigint unsigned NOT NULL DEFAULT 0", false},             // 少了 unsigned 会截半个值域
		{"mediumblob", "MEDIUMBLOB COMMENT 'pb:3'", true},
		{"tinyint(1)", "tinyint(1) NOT NULL DEFAULT 0", true},
		{"int unsigned", "int unsigned NOT NULL DEFAULT 0 COMMENT 'pb:4'", true},
		{"int", "int NOT NULL DEFAULT 0 COMMENT 'pb:4'", true},
		{"varchar(191)", "varchar(255) NOT NULL", true}, // 只比类型族,不比长度
		{"varchar(191)", "MEDIUMTEXT COMMENT 'pb:5'", false},
		{"mediumtext", "MEDIUMBLOB", false},
		{"datetime(6)", "DATETIME(6) COMMENT 'pb:9'", true},
		{"double", "double NOT NULL DEFAULT 0", true},
	}
	for _, tc := range cases {
		if got := typesCompatible(tc.current, tc.target); got != tc.want {
			t.Errorf("typesCompatible(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

func TestDriftCleanWhenLiveMatchesProto(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t), favoriteTable().build(t), receiptTable().build(t))
	d := sch.drift(liveMatching(sch), driftOptions{CreateMissingTables: true})
	if !d.migration.Empty() || len(d.warnings) != 0 || len(d.manual) != 0 {
		t.Fatalf("一致的库不应产生任何语句或报告项: %+v", d)
	}
}

func TestDriftAddsMissingColumns(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t))
	live := liveMatching(sch)
	dropLiveColumn(live["trade_listing"], "title")
	dropLiveColumn(live["trade_listing"], "price_fen")

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	if len(d.migration.Statements) != 2 {
		t.Fatalf("missing columns must produce ADD COLUMN statements, got %v", d.migration.Statements)
	}
	for _, s := range d.migration.Statements {
		if !strings.HasPrefix(s, "ALTER TABLE `trade_listing` ADD COLUMN ") {
			t.Fatalf("drift must only emit ADD COLUMN by default, got: %s", s)
		}
	}
	if !strings.Contains(strings.Join(d.migration.Statements, "\n"), "ADD COLUMN `title` MEDIUMTEXT COMMENT 'pb:5'") {
		t.Fatalf("补出来的列必须与建表时的列定义逐字相同(含 pb:N 注释),got %v", d.migration.Statements)
	}
	if d.migration.Version != 0 {
		t.Fatalf("drift version must be allocated by the runner, got %d", d.migration.Version)
	}
	if d.migration.Checksum == "" || d.migration.Name != driftNamePrefix+d.migration.Checksum[:12] {
		t.Fatalf("drift identity must come from its checksum, got name=%s checksum=%s", d.migration.Name, d.migration.Checksum)
	}
	if len(d.warnings) != 0 || len(d.manual) != 0 {
		t.Fatalf("pure additions must not need review: warnings=%v manual=%v", d.warnings, d.manual)
	}
}

func TestDriftReportsTypeMismatchAsManualInsteadOfModifying(t *testing.T) {
	// proto2mysql 的 CreateOrUpdateTable 在类型不匹配时会直接 MODIFY COLUMN 并执行 ——
	// 大表上是重建表级别的在线 DDL,默认改为只报告,且进 Manual(阻断)。
	sch := mustSchema(t, listingTable().build(t))
	live := liveMatching(sch)
	live["trade_listing"].columns["price_fen"] = "varchar(64)"

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	if !d.migration.Empty() {
		t.Fatalf("type drift must not be auto-applied without AllowModifyColumn: %v", d.migration.Statements)
	}
	if len(d.manual) != 1 || !containsAll(d.manual[0], "列类型漂移", "trade_listing.price_fen") {
		t.Fatalf("type drift must be reported as manual, got %v", d.manual)
	}

	// 显式授权后才生成 MODIFY COLUMN。
	d = sch.drift(live, driftOptions{CreateMissingTables: true, AllowModifyColumn: true})
	if len(d.manual) != 0 {
		t.Fatalf("授权后类型漂移不应再进 Manual: %v", d.manual)
	}
	if len(d.migration.Statements) != 1 || !strings.HasPrefix(d.migration.Statements[0], "ALTER TABLE `trade_listing` MODIFY COLUMN `price_fen` bigint unsigned") {
		t.Fatalf("AllowModifyColumn must emit MODIFY COLUMN, got %v", d.migration.Statements)
	}
}

func TestDriftReportsPrimaryKeyProblemsAndExtraColumns(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t), favoriteTable().build(t))
	live := liveMatching(sch)
	live["trade_listing"].primaryKey = nil
	live.addColumn("trade_listing", "legacy_column", "mediumblob")
	live["trade_favorite"].primaryKey = []string{"listing_id", "player_id"}

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	manual := strings.Join(d.manual, "\n")
	if !containsAll(manual, "缺主键", "trade_listing") {
		t.Fatalf("missing primary key must be manual, got %v", d.manual)
	}
	if !containsAll(manual, "主键列不符", "trade_favorite") {
		t.Fatalf("primary key column mismatch must be manual, got %v", d.manual)
	}
	warnings := strings.Join(d.warnings, "\n")
	if !containsAll(warnings, "多余列", "trade_listing.legacy_column") {
		t.Fatalf("extra columns must be reported (and never dropped), got %v", d.warnings)
	}
	if strings.Contains(manual, "legacy_column") {
		t.Fatalf("多余列不能阻断(回滚到旧版本时是正常形态): %v", d.manual)
	}
	if !d.migration.Empty() {
		t.Fatalf("主键问题与多余列都不能生成语句: %v", d.migration.Statements)
	}
}

func TestDriftReportsMissingIndexesAndUniqueKey(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t), receiptTable().build(t))
	live := liveMatching(sch)
	delete(live["trade_listing"].indexes, "idx_trade_listing_1")
	delete(live["trade_receipt"].indexes, "uk_trade_receipt")

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	if !containsAll(strings.Join(d.warnings, "\n"), "缺索引", "idx_trade_listing_1") {
		t.Fatalf("missing index must be a warning, got %v", d.warnings)
	}
	if !containsAll(strings.Join(d.manual, "\n"), "缺唯一键", "uk_trade_receipt") {
		t.Fatalf("missing unique key must be manual, got %v", d.manual)
	}
	if !d.migration.Empty() {
		t.Fatalf("索引问题不自动修: %v", d.migration.Statements)
	}
}

// 缺陷回归(D-14 第 3 条):基线跑过之后再往表清单加表,漂移必须把新表建出来,而不是只打 WARN。
func TestDriftCreatesTablesMissingFromLiveSchema(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t), favoriteTable().build(t))
	live := liveMatching(mustSchema(t, listingTable().build(t)))

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	if len(d.migration.Statements) != 1 || d.migration.Statements[0] != sch.tables[1].createSQL {
		t.Fatalf("缺失的表必须按 proto 建出来,got %v", d.migration.Statements)
	}
	if len(d.manual) != 0 || len(d.warnings) != 0 {
		t.Fatalf("后加表是正常演进,不应有报告项: warnings=%v manual=%v", d.warnings, d.manual)
	}

	// 基线尚未执行的 Plan:缺的表由基线建,漂移不重复列出。
	d = sch.drift(live, driftOptions{CreateMissingTables: false})
	if !d.migration.Empty() {
		t.Fatalf("CreateMissingTables=false 时不应生成建表语句: %v", d.migration.Statements)
	}
}

func TestDriftTreatsExtraTablesAsWarnings(t *testing.T) {
	sch := mustSchema(t, listingTable().build(t))
	live := liveMatching(mustSchema(t, listingTable().build(t), favoriteTable().build(t)))
	live.addColumn(SchemaMigrationsTable, "version", "bigint unsigned")

	d := sch.drift(live, driftOptions{CreateMissingTables: true})
	if len(d.manual) != 0 {
		t.Fatalf("多余表不能阻断(回滚到旧版本二进制时是正常形态): %v", d.manual)
	}
	if len(d.warnings) != 1 || !containsAll(d.warnings[0], "多余表", "trade_favorite") {
		t.Fatalf("多余表必须进 Warnings,台账表本身不算,got %v", d.warnings)
	}
}

func TestDriftChecksumIsOrderIndependent(t *testing.T) {
	a := mustSchema(t, listingTable().build(t), favoriteTable().build(t))
	b := mustSchema(t, favoriteTable().build(t), listingTable().build(t))
	empty := liveSchema{}
	da := a.drift(empty, driftOptions{CreateMissingTables: true})
	db := b.drift(empty, driftOptions{CreateMissingTables: true})
	if da.migration.Checksum == "" || da.migration.Checksum != db.migration.Checksum {
		t.Fatalf("漂移身份必须与表清单顺序无关: %s vs %s", da.migration.Checksum, db.migration.Checksum)
	}
}

func TestLoadLiveSchemaIgnoresIndexRowsOfUnknownTables(t *testing.T) {
	live := liveSchema{}
	live.addIndexColumn("ghost", "PRIMARY", "id")
	if _, ok := live["ghost"]; ok {
		t.Fatal("只在 STATISTICS 出现、COLUMNS 里没有的表不能被当成存在(会生成整表 ADD COLUMN)")
	}
	live.addColumn("t", "a", "int")
	live.addColumn("t", "b", "int")
	live.addIndexColumn("t", "PRIMARY", "b")
	live.addIndexColumn("t", "PRIMARY", "a")
	if !slices.Equal(live["t"].primaryKey, []string{"b", "a"}) || !slices.Equal(live["t"].columnOrder, []string{"a", "b"}) {
		t.Fatalf("主键列按 SEQ_IN_INDEX、列按 ORDINAL_POSITION 保序: %+v", live["t"])
	}
}
