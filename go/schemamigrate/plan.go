package schemamigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// SchemaMigrationsTable 是版本台账表名,与 go/db 相同。
const SchemaMigrationsTable = "schema_migrations"

// BaselineVersion 是「按表清单建全套表」这条迁移的固定版本号,与 go/db 相同。
const BaselineVersion int64 = 1

// BaselineName 是基线迁移在台账里的名字,与 go/db 相同。
const BaselineName = "0001_baseline_from_proto"

// driftNamePrefix 是漂移迁移在台账里的名字前缀,与 go/db 相同。
const driftNamePrefix = "auto_proto_sync_"

// tableNameRe 限定表名字符集与长度(MySQL / TiDB 标识符上限 64)。
// 表名来自 proto 选项而非玩家输入,但 DDL 无法参数化,收紧字符集比事后转义更省心。
var tableNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// createTableNameRe 从 proto2mysql 生成的建表语句里解出表名,用于表名守卫。
var createTableNameRe = regexp.MustCompile("^CREATE TABLE IF NOT EXISTS `([^`]+)`")

// backtickNameRe 解出一行里所有反引号包裹的标识符。
var backtickNameRe = regexp.MustCompile("`([^`]+)`")

// migration 是一条可执行的迁移。
//
// Version = 0 表示「由 runner 在执行前分配下一个可用版本号」,用于按实际库结构算出来的
// 漂移迁移 —— 它的身份由 Checksum 决定,不由人工编号决定。
type migration struct {
	Version    int64
	Name       string
	Checksum   string
	Statements []string
}

// Empty 报告这条迁移是否无事可做。
func (m migration) Empty() bool { return len(m.Statements) == 0 }

// columnSpec 是 proto 声明出来的一列。definition 是 proto2mysql 生成的整段列定义
// (类型 + NOT NULL / DEFAULT + COMMENT 'pb:N'),ADD / MODIFY COLUMN 原样使用,
// 保证补出来的列与建表时建出来的列逐字相同(pb:N 注释是 proto2mysql 按字段号识别列的依据)。
type columnSpec struct {
	name       string
	definition string
}

// tableSpec 是一张表的全部期望结构,全部从 proto2mysql 生成的建表语句反解,不自己再实现类型映射
// —— 自己抄一份映射必然与库漂移,后果是永远报「类型不一致」的假阳性。
type tableSpec struct {
	message    protoreflect.FullName
	name       string
	createSQL  string
	columns    []columnSpec
	primaryKey []string
	indexes    []string // 普通索引名(proto2mysql 命名为 idx_<表名>_<序号>)
	uniqueKey  string   // 唯一键名(uk_<表名>);没有则为空
}

// schema 是 Options.Tables 校验通过后的表清单,顺序即建表顺序。
type schema struct {
	tables []tableSpec
}

// buildSchema 校验表清单并从 proto 推导每张表的期望结构。纯内存,不碰库。
//
// 这里的失败都是代码 / proto 写错(ExitCode=1),不是需人工处理的库状态:
// 空清单、nil 元素、缺 OptionTableName、表名非法或重复、字段类型没有列映射、
// 缺主键或主键不是整数 / 枚举列(D-14 第 2 条)。
func buildSchema(messages []proto.Message) (*schema, error) {
	if len(messages) == 0 {
		return nil, errors.New("schemamigrate: Options.Tables 必填且不能为空")
	}
	sch := &schema{tables: make([]tableSpec, 0, len(messages))}
	owners := make(map[string]protoreflect.FullName, len(messages))
	for i, msg := range messages {
		if msg == nil {
			return nil, fmt.Errorf("schemamigrate: Options.Tables[%d] 为 nil", i)
		}
		spec, err := buildTable(msg)
		if err != nil {
			return nil, err
		}
		if prev, dup := owners[spec.name]; dup {
			return nil, fmt.Errorf("schemamigrate: 表名 %q 重复:%s 与 %s 声明了同一个 OptionTableName", spec.name, prev, spec.message)
		}
		owners[spec.name] = spec.message
		sch.tables = append(sch.tables, spec)
	}
	return sch, nil
}

// buildTable 推导单张表的期望结构。
func buildTable(msg proto.Message) (tableSpec, error) {
	md := msg.ProtoReflect().Descriptor()
	spec := tableSpec{message: md.FullName()}

	// 表名守卫:proto2mysql 没读到 OptionTableName 时表名退化成 proto full name(带 package),
	// 迁移会静默建出一张新空表,存量数据对业务「消失」。所以必须显式声明。
	name, ok := proto2mysql.TableNameFromDescriptor(md)
	if !ok {
		return spec, fmt.Errorf("schemamigrate: 表名守卫:消息 %s 未声明 OptionTableName,拒绝按 proto full name 建表", md.FullName())
	}
	if !tableNameRe.MatchString(name) {
		return spec, fmt.Errorf("schemamigrate: 消息 %s 的表名 %q 非法(只允许字母、数字、下划线,1-64 字符)", md.FullName(), name)
	}
	if name == SchemaMigrationsTable {
		return spec, fmt.Errorf("schemamigrate: 消息 %s 的表名 %q 与迁移台账表重名", md.FullName(), name)
	}
	if err := checkFieldKinds(md, name); err != nil {
		return spec, err
	}

	ddl := strings.TrimSpace(proto2mysql.GenerateCreateTableSQL(msg))
	ddl = strings.TrimSpace(strings.TrimSuffix(ddl, ";"))
	m := createTableNameRe.FindStringSubmatch(ddl)
	if m == nil {
		return spec, fmt.Errorf("schemamigrate: 表名守卫:无法从消息 %s 的建表语句中解析表名: %s", md.FullName(), collapse(ddl))
	}
	if m[1] != name {
		return spec, fmt.Errorf("schemamigrate: 表名守卫:消息 %s 生成的表名 %q 与 OptionTableName %q 不一致", md.FullName(), m[1], name)
	}
	spec.name = name
	spec.createSQL = ddl

	lines := strings.Split(ddl, "\n")
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		col := string(fields.Get(i).Name())
		def, ok := columnDefinition(lines, col)
		if !ok {
			// proto2mysql 输出格式变了而本包没跟上:宁可拒绝,也不带着「查不到列定义」去比对漂移。
			return spec, fmt.Errorf("schemamigrate: 无法从表 %s 的建表语句中解析列 %s 的定义(proto2mysql 输出格式与本包解析不符): %s",
				name, col, collapse(ddl))
		}
		spec.columns = append(spec.columns, columnSpec{name: col, definition: def})
	}
	spec.primaryKey, spec.indexes, spec.uniqueKey = parseKeys(lines)

	if len(spec.primaryKey) == 0 {
		return spec, fmt.Errorf("schemamigrate: 表 %s(消息 %s)未声明 OptionPrimaryKey;D-14 要求新服务的表显式声明整数主键", name, md.FullName())
	}
	for _, col := range spec.primaryKey {
		fd := fields.ByName(protoreflect.Name(col))
		if fd == nil {
			return spec, fmt.Errorf("schemamigrate: 表 %s 的主键列 %s 不是消息 %s 的字段", name, col, md.FullName())
		}
		if !isIntegerKeyField(fd) {
			return spec, fmt.Errorf("schemamigrate: 表 %s 的主键列 %s 类型为 %s;D-14 第 2 条只允许整数或枚举列作主键(禁止 string / bytes 主键)",
				name, col, fieldKindLabel(fd))
		}
	}
	return spec, nil
}

// checkFieldKinds 拒绝没有 MySQL 列类型映射的字段(sint* / fixed* / sfixed*)。
//
// proto2mysql 的 GetCreateTableSQL 对这些类型静默回落成 TEXT,建表一路成功,第一次写入才失败,
// 那时列已经建出来了。它自己的校验(validateFieldKinds)不对外暴露,所以这里按同一张映射表再拦一次。
func checkFieldKinds(md protoreflect.MessageDescriptor, table string) error {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() || fd.Kind() == protoreflect.MessageKind {
			continue // 这三类统一落 MEDIUMBLOB / DATETIME(6),不查映射表
		}
		if _, ok := proto2mysql.MySQLFieldTypes[fd.Kind()]; !ok {
			return fmt.Errorf("schemamigrate: 表 %s 的字段 %s 类型 %s 没有 MySQL 列类型映射;改用 int32 / int64 / uint32 / uint64",
				table, fd.Name(), fd.Kind())
		}
	}
	return nil
}

// isIntegerKeyField 判断字段能否作主键列:单值的整数或枚举。
func isIntegerKeyField(fd protoreflect.FieldDescriptor) bool {
	if fd.IsList() || fd.IsMap() {
		return false
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Uint32Kind, protoreflect.Uint64Kind, protoreflect.EnumKind:
		return true
	default:
		return false
	}
}

func fieldKindLabel(fd protoreflect.FieldDescriptor) string {
	switch {
	case fd.IsMap():
		return "map"
	case fd.IsList():
		return "repeated " + fd.Kind().String()
	default:
		return fd.Kind().String()
	}
}

// columnDefinition 在建表语句里找 "  `列名` <定义>" 这一行,返回去掉尾逗号的定义。
func columnDefinition(lines []string, column string) (string, bool) {
	prefix := "  " + quoteIdent(column) + " "
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		def := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, prefix)), ","))
		if def == "" {
			return "", false
		}
		return def, true
	}
	return "", false
}

// parseKeys 从建表语句里解出主键列、普通索引名与唯一键名。
func parseKeys(lines []string) (primaryKey []string, indexes []string, uniqueKey string) {
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "  PRIMARY KEY ("):
			// 形如 "  PRIMARY KEY (`a`,`b`) /*T![clustered_index] NONCLUSTERED */,":TiDB 注释里没有反引号。
			for _, m := range backtickNameRe.FindAllStringSubmatch(line, -1) {
				primaryKey = append(primaryKey, m[1])
			}
		case strings.HasPrefix(line, "  INDEX `"):
			if m := backtickNameRe.FindStringSubmatch(line); m != nil {
				indexes = append(indexes, m[1])
			}
		case strings.HasPrefix(line, "  UNIQUE KEY `"):
			if m := backtickNameRe.FindStringSubmatch(line); m != nil {
				uniqueKey = m[1]
			}
		}
	}
	return primaryKey, indexes, uniqueKey
}

// baseline 生成 version=1 的建表基线:全部是 CREATE TABLE IF NOT EXISTS,对已存在的表是幂等的。
//
// checksum 只算表名集合(排序后):proto 加一个字段就会改变建表语句文本,如果 checksum 跟着变,
// 每次改 proto 都会被当成「基线变了」。列级变更与后加的表都走漂移迁移。
func (s *schema) baseline() migration {
	names := make([]string, 0, len(s.tables))
	statements := make([]string, 0, len(s.tables))
	for _, t := range s.tables {
		names = append(names, t.name)
		statements = append(statements, t.createSQL)
	}
	sort.Strings(names)
	return migration{
		Version:    BaselineVersion,
		Name:       BaselineName,
		Checksum:   checksum(names),
		Statements: statements,
	}
}

// driftOptions 控制漂移计算。
type driftOptions struct {
	// AllowModifyColumn 为 true 时把列类型漂移变成 MODIFY COLUMN;默认只进 Manual。
	AllowModifyColumn bool
	// CreateMissingTables 为 true 时对库里查不到的表生成 CREATE TABLE IF NOT EXISTS
	// (修「后加表建不出来」);基线尚未执行的 Plan 传 false,避免与基线语句重复。
	CreateMissingTables bool
}

// driftResult 是一次漂移计算的结果。
type driftResult struct {
	migration migration // Version 为 0,由 runner 分配
	warnings  []string
	manual    []string
}

// drift 比对真实库结构与 proto 期望,产出可执行语句与报告项。
//
// 可执行语句只有两类:缺表 → CREATE TABLE IF NOT EXISTS;缺列 → ADD COLUMN(授权后外加 MODIFY COLUMN)。
// 它们都不会丢数据,且在新服务的小表上代价可控。其余差异一律只报告:
//   - Manual(阻断启动,ExitCode=4):列类型漂移、缺主键、主键列不符、缺唯一键。
//     缺唯一键会让 INSERT IGNORE / ON DUPLICATE KEY 的幂等语义失效,补唯一键前要先去重,只能人工做。
//   - Warnings(不阻断):多余列、多余表、缺普通索引。多余列 / 多余表是回滚到旧版本二进制时的正常
//     形态(新版本加的列 / 表旧版本不认识),把它们当阻断项会让回滚后的副本起不来;缺普通索引只影响性能。
func (s *schema) drift(live liveSchema, o driftOptions) driftResult {
	var res driftResult
	var creates, alters []string
	declared := make(map[string]struct{}, len(s.tables))

	for _, t := range s.tables {
		declared[t.name] = struct{}{}
		lt, ok := live[t.name]
		if !ok {
			if o.CreateMissingTables {
				creates = append(creates, t.createSQL)
			}
			continue
		}

		known := make(map[string]struct{}, len(t.columns))
		for _, col := range t.columns {
			known[col.name] = struct{}{}
			current, exists := lt.columns[col.name]
			switch {
			case !exists:
				alters = append(alters, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", quoteIdent(t.name), quoteIdent(col.name), col.definition))
			case typesCompatible(current, col.definition):
			case o.AllowModifyColumn:
				alters = append(alters, fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s", quoteIdent(t.name), quoteIdent(col.name), col.definition))
			default:
				res.manual = append(res.manual, fmt.Sprintf(
					"列类型漂移(未自动修改):%s.%s 现为 %q,proto 期望 %q;确认影响面后以 AllowModifyColumn=true 重跑,或人工 ALTER",
					t.name, col.name, current, col.definition))
			}
		}
		for _, name := range lt.columnOrder {
			if _, ok := known[name]; !ok {
				res.warnings = append(res.warnings, fmt.Sprintf("多余列(不会删除):%s.%s 不在 proto 声明里", t.name, name))
			}
		}

		switch {
		case len(lt.primaryKey) == 0:
			res.manual = append(res.manual, fmt.Sprintf(
				"缺主键:%s 按 proto 应有 PRIMARY KEY (%s),实际没有;主键去重语义失效,需人工补主键(有重复行时先去重)",
				t.name, strings.Join(t.primaryKey, ",")))
		case !slices.Equal(lt.primaryKey, t.primaryKey):
			res.manual = append(res.manual, fmt.Sprintf(
				"主键列不符:%s 实际 PRIMARY KEY (%s),proto 期望 (%s);需人工核对",
				t.name, strings.Join(lt.primaryKey, ","), strings.Join(t.primaryKey, ",")))
		}
		for _, idx := range t.indexes {
			if _, ok := lt.indexes[idx]; !ok {
				res.warnings = append(res.warnings, fmt.Sprintf("缺索引(不会自动建):%s 上没有 proto 声明的索引 %s", t.name, idx))
			}
		}
		if t.uniqueKey != "" {
			if _, ok := lt.indexes[t.uniqueKey]; !ok {
				res.manual = append(res.manual, fmt.Sprintf(
					"缺唯一键:%s 上没有 proto 声明的唯一键 %s;去重语义失效,需人工补建(有重复行时先去重)", t.name, t.uniqueKey))
			}
		}
	}

	for _, name := range live.tableNames() {
		if name == SchemaMigrationsTable {
			continue
		}
		if _, ok := declared[name]; !ok {
			res.warnings = append(res.warnings, fmt.Sprintf("多余表(不会删除):%s 不在表清单里", name))
		}
	}
	sort.Strings(res.warnings)
	sort.Strings(res.manual)

	// 执行顺序:先建缺的表,再改已有的表;两类互不依赖(不建外键)。身份按排序后的集合算,与顺序无关。
	statements := append(creates, alters...)
	if len(statements) > 0 {
		sorted := slices.Clone(statements)
		sort.Strings(sorted)
		sum := checksum(sorted)
		res.migration = migration{
			Name:       driftNamePrefix + sum[:12],
			Checksum:   sum,
			Statements: statements,
		}
	}
	return res
}

// liveTable 是库里一张表的真实结构。
type liveTable struct {
	columns     map[string]string // 列名 → COLUMN_TYPE(含长度与 unsigned)
	columnOrder []string          // 按 ORDINAL_POSITION
	primaryKey  []string          // 按 SEQ_IN_INDEX
	indexes     map[string]struct{}
}

// liveSchema 是目标库的真实结构,键为表名。
type liveSchema map[string]*liveTable

// addColumn 记录一列;表第一次出现时建出条目。
func (l liveSchema) addColumn(table, column, columnType string) {
	t, ok := l[table]
	if !ok {
		t = &liveTable{columns: map[string]string{}, indexes: map[string]struct{}{}}
		l[table] = t
	}
	if _, dup := t.columns[column]; !dup {
		t.columnOrder = append(t.columnOrder, column)
	}
	t.columns[column] = columnType
}

// addIndexColumn 记录一条索引里的一列。只认已经在 COLUMNS 里出现过的表。
func (l liveSchema) addIndexColumn(table, index, column string) {
	t, ok := l[table]
	if !ok {
		return
	}
	t.indexes[index] = struct{}{}
	if index == "PRIMARY" && column != "" {
		t.primaryKey = append(t.primaryKey, column)
	}
}

// tableNames 返回排好序的表名。
func (l liveSchema) tableNames() []string {
	names := make([]string, 0, len(l))
	for name := range l {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// queryer 是 *sql.DB / *sql.Conn 的查询子集。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadLiveSchema 读出目标库全部表的列、主键与索引。
func loadLiveSchema(ctx context.Context, q queryer, database string) (liveSchema, error) {
	live := liveSchema{}

	rows, err := q.QueryContext(ctx,
		"SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION",
		database)
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: 读取库 %q 的 information_schema.COLUMNS 失败: %w", database, err)
	}
	for rows.Next() {
		var table, column, columnType string
		if err := rows.Scan(&table, &column, &columnType); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("schemamigrate: 解析库 %q 的 information_schema.COLUMNS 失败: %w", database, err)
		}
		live.addColumn(table, column, columnType)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("schemamigrate: 遍历库 %q 的 information_schema.COLUMNS 失败: %w", database, err)
	}

	rows, err = q.QueryContext(ctx,
		"SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX",
		database)
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: 读取库 %q 的 information_schema.STATISTICS 失败: %w", database, err)
	}
	for rows.Next() {
		var table, index string
		var column sql.NullString // MySQL 8 的函数索引没有列名
		if err := rows.Scan(&table, &index, &column); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("schemamigrate: 解析库 %q 的 information_schema.STATISTICS 失败: %w", database, err)
		}
		live.addIndexColumn(table, index, column.String)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("schemamigrate: 遍历库 %q 的 information_schema.STATISTICS 失败: %w", database, err)
	}
	return live, nil
}

// closeRows 关闭结果集并返回遍历期间的错误。
func closeRows(rows *sql.Rows) error {
	iterErr := rows.Err()
	closeErr := rows.Close()
	if iterErr != nil {
		return iterErr
	}
	return closeErr
}

// typesCompatible 判断库里的真实列类型是否已经满足 proto 声明。
//
// 只比「基础类型 + unsigned」,不比长度、默认值与注释:长度差异(int(10) vs int,TiDB 会带显示宽度)
// 报出来只会制造噪音,真正危险的是基础类型换了族(varchar → mediumtext 会让主键做不了)。
func typesCompatible(current, target string) bool {
	c := parseColumnType(current)
	t := parseColumnType(target)
	return c.base == t.base && c.unsigned == t.unsigned
}

type columnType struct {
	base     string
	unsigned bool
}

// parseColumnType 把 "bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:1'" / "tinyint(1)"
// 之类的串归一成 {base, unsigned}。
func parseColumnType(raw string) columnType {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, cut := range []string{" comment ", " default "} {
		if idx := strings.Index(s, cut); idx >= 0 {
			s = s[:idx]
		}
	}
	for _, kw := range []string{" not null", " null", " auto_increment", " zerofill"} {
		s = strings.ReplaceAll(s, kw, "")
	}
	// 去掉长度 / 精度括号,包括 enum('a','b') 这种带逗号的。
	if open := strings.IndexByte(s, '('); open >= 0 {
		if end := strings.LastIndexByte(s, ')'); end > open {
			s = s[:open] + s[end+1:]
		}
	}
	fields := strings.Fields(s)
	out := columnType{}
	if len(fields) > 0 {
		out.base = fields[0]
	}
	for _, f := range fields[1:] {
		if f == "unsigned" {
			out.unsigned = true
		}
	}
	return out
}

// quoteIdent 反引号转义标识符。
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// checksum 对一组字符串算 sha256(每项后接换行),口径与 go/db 相同。
func checksum(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// shortSum 取 checksum 前 12 位,用于日志。
func shortSum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// collapse 把多行 DDL 压成一行,方便进日志与报告。
func collapse(stmt string) string {
	return strings.Join(strings.Fields(stmt), " ")
}

// collapseAll 对一组语句逐条 collapse。
func collapseAll(statements []string) []string {
	out := make([]string, len(statements))
	for i, s := range statements {
		out[i] = collapse(s)
	}
	return out
}
