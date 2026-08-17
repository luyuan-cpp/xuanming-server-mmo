// Package migrate 是 db 的**版本化 schema 迁移**执行器。
//
// 它存在的理由是把 DDL 从业务服务的启动路径上摘下来:
//
//   - 旧行为里 InitDB() 会对 11 张表逐张跑 CreateOrUpdateTable。多 zone ×
//     多副本同时启动时,这些 ALTER 会并发落到同一张大表上,MDL 阻塞会让整个
//     zone 的 db_task 消费停摆;
//   - proto2mysql 在列类型不匹配时会自动拼 MODIFY COLUMN 并 Exec,没有版本
//     记录、没有 lock 超时 —— 一次无人批准的在线 DDL。
//
// 本包提供的保证:
//
//  1. 版本化 —— schema_migrations 记录 version / name / checksum / dirty;
//  2. 只跑 up —— 没有 down,拒绝在 dirty 状态下继续;
//  3. 跨实例互斥 —— GET_LOCK 按库名加锁,多副本 / 多 zone 不会并发 ALTER;
//  4. 有超时 —— 会话级 lock_wait_timeout / innodb_lock_wait_timeout,
//     外加每条语句的硬超时 + 旁路 KILL QUERY;
//  5. 不自动改列类型 —— 类型漂移默认只报告,要显式 -allow-modify 才生成
//     MODIFY COLUMN。
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	dbpb "proto/db"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// BaselineVersion 是「按 proto 建全套表」这条迁移的固定版本号。
const BaselineVersion int64 = 1

// BaselineName 是基线迁移在 schema_migrations 里的名字。
const BaselineName = "0001_baseline_from_proto"

// Migration 是一条可执行的迁移。
//
// Version = 0 表示「由 runner 在提交时分配下一个可用版本号」,用于按实际
// 库结构算出来的漂移迁移 —— 它的身份由 Checksum 决定,不由人工编号决定。
type Migration struct {
	Version    int64
	Name       string
	Checksum   string
	Statements []string
}

// Empty 报告这条迁移是否无事可做。
func (m Migration) Empty() bool { return len(m.Statements) == 0 }

// Queryer 是 *sql.DB / *sql.Conn / *sql.Tx 的公共查询子集。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Source 提供迁移内容。
//
// Drift 刻意在 Baseline **执行之后**才被调用:新建的表要先落地,漂移比对
// 看到的才是真实列结构,而不是「建表前的空集」。
type Source interface {
	Baseline() (Migration, error)
	Drift(ctx context.Context, q Queryer, database string) (Migration, []string, error)
}

// ProtoSource 从 proto 表清单推导迁移内容。
//
// 为什么不手写 SQL 迁移文件:仓库 CLAUDE.md §3 禁止手写与 proto 重复的并行
// 定义,手抄一份 DDL 必然和 proto 漂移。代价是基线的 SQL 文本会随 proto 变化,
// 所以基线的 checksum 只算**表名集合**(表增删才是基线级变更),列级变更走
// 漂移迁移,由它自己的 checksum 去重。
type ProtoSource struct {
	// Model 必须已经 RegisterTable 过全部 Tables,否则拿不到建表语句。
	Model *proto2mysql.PbMysqlDB
	// Tables 是表清单,顺序即建表顺序。
	Tables []proto.Message
	// AllowModifyColumn 为 true 时把「列类型漂移」也变成可执行的 MODIFY COLUMN。
	// 默认 false —— 大表上的 MODIFY COLUMN 是重建表级别的操作,必须人工过目。
	AllowModifyColumn bool
}

// Baseline 生成 version=1 的建表基线。全部是 CREATE TABLE IF NOT EXISTS,
// 对已存在的库重复执行是幂等的。
func (s *ProtoSource) Baseline() (Migration, error) {
	if s.Model == nil {
		return Migration{}, fmt.Errorf("proto source has no registered model")
	}
	names := make([]string, 0, len(s.Tables))
	statements := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		name := proto2mysql.GetTableName(t)
		ddl := strings.TrimSpace(s.Model.GetCreateTableSQL(t))
		if ddl == "" {
			return Migration{}, fmt.Errorf("table %s is not registered in the proto2mysql model", name)
		}
		ddl, err := ensurePrimaryKey(ddl, declaredPrimaryKey(s.Model, t), name)
		if err != nil {
			return Migration{}, err
		}
		names = append(names, name)
		statements = append(statements, ddl)
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return Migration{
		Version:    BaselineVersion,
		Name:       BaselineName,
		Checksum:   checksum(sorted),
		Statements: statements,
	}, nil
}

// Drift 比对 information_schema 里的真实列结构与 proto 声明。
//
// 只有「缺列」会变成可执行语句(ADD COLUMN 是唯一在大表上代价可控、且不会
// 丢数据的变更)。类型漂移、多余列、缺主键一律只进 warnings,交人工决定。
func (s *ProtoSource) Drift(ctx context.Context, q Queryer, database string) (Migration, []string, error) {
	existing, err := LoadColumns(ctx, q, database)
	if err != nil {
		return Migration{}, nil, err
	}
	primaries, err := LoadPrimaryKeyTables(ctx, q, database)
	if err != nil {
		return Migration{}, nil, err
	}

	var statements []string
	var warnings []string
	for _, t := range s.Tables {
		table := proto2mysql.GetTableName(t)
		columns, ok := existing[table]
		if !ok {
			// 基线已经跑过还查不到,说明建表被跳过或权限不足,必须显式报出来。
			warnings = append(warnings, fmt.Sprintf("表 %s 在 information_schema 里查不到,基线建表可能失败", table))
			continue
		}

		declared := map[string]struct{}{}
		for _, col := range describeColumns(s.Model, t) {
			declared[col.Name] = struct{}{}
			current, exists := columns[col.Name]
			if !exists {
				statements = append(statements,
					fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", quoteIdent(table), quoteIdent(col.Name), col.Type))
				continue
			}
			if TypesCompatible(current, col.Type) {
				continue
			}
			if s.AllowModifyColumn {
				statements = append(statements,
					fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s %s", quoteIdent(table), quoteIdent(col.Name), col.Type))
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"列类型漂移(未自动修改):%s.%s 现为 %q,proto 期望 %q。确认无误后加 -allow-modify 重跑",
				table, col.Name, current, col.Type))
		}
		for name := range columns {
			if _, ok := declared[name]; !ok {
				warnings = append(warnings, fmt.Sprintf(
					"多余列(不会删除):%s.%s 不在 proto 声明里", table, name))
			}
		}

		if pk := declaredPrimaryKey(s.Model, t); pk != "" {
			if _, ok := primaries[table]; !ok {
				warnings = append(warnings, fmt.Sprintf(
					"缺主键:%s 按 proto 应有 PRIMARY KEY (%s),实际没有。"+
						"INSERT ... ON DUPLICATE KEY UPDATE 的幂等语义会整个失效(每次存盘追加新行),"+
						"需人工 ALTER TABLE 补主键", table, pk))
			}
		}
	}

	sort.Strings(warnings)
	if len(statements) == 0 {
		return Migration{}, warnings, nil
	}
	sort.Strings(statements)
	sum := checksum(statements)
	return Migration{
		Name:       "auto_proto_sync_" + sum[:12],
		Checksum:   sum,
		Statements: statements,
	}, warnings, nil
}

// ColumnSpec 是 proto 声明出来的一列。
type ColumnSpec struct {
	Name string
	Type string
}

// createColumnRe 从 proto2mysql 生成的建表语句里逐列解析出「列名 + 类型」。
//
// 之所以从生成的 SQL 反解而不是自己再实现一遍类型映射:proto2mysql 的映射
// 里有 nullable / AUTO_INCREMENT 等特例,自己抄一份必然漂移,而漂移的后果是
// 迁移工具永远报「类型不一致」的假阳性。
var createColumnRe = regexp.MustCompile("(?m)^\\s{2}(?:`([^`]+)`|([A-Za-z0-9_]+))\\s+(.+?),?$")

// describeColumns 反解建表语句,返回 proto 声明的列。
func describeColumns(model *proto2mysql.PbMysqlDB, msg proto.Message) []ColumnSpec {
	ddl := model.GetCreateTableSQL(msg)
	if ddl == "" {
		return nil
	}
	declared := map[string]struct{}{}
	fields := msg.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		declared[string(fields.Get(i).Name())] = struct{}{}
	}

	var out []ColumnSpec
	for _, m := range createColumnRe.FindAllStringSubmatch(ddl, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		// PRIMARY KEY / INDEX / UNIQUE KEY 行也匹配这个形状,用 proto 字段名过滤。
		if _, ok := declared[name]; !ok {
			continue
		}
		out = append(out, ColumnSpec{Name: name, Type: strings.TrimSpace(strings.TrimSuffix(m[3], ","))})
	}
	return out
}

var primaryKeyRe = regexp.MustCompile(`PRIMARY KEY \(([^)]*)\)`)

// declaredPrimaryKey 返回 proto 声明的主键列(逗号分隔),没有则返回空串。
//
// **权威顺序刻意是「proto 选项 → 建表 SQL」而不是反过来。**
//
// 原因是 proto2mysql v0.0.18 的 GetCreateTableSQL 对 player_database 这类表
// 根本不输出 PRIMARY KEY 子句(实测:整条 CREATE 里没有 PRIMARY KEY),而
// go/db/model/mysql_database_table.sql 里它明明写着 PRIMARY KEY (player_id)。
// 两个"真源"不一致。若只按建表 SQL 反向正则:
//
//   - 缺主键告警永远不可能触发(regex 永远匹配不到),这条守卫等于没写;
//   - 更糟的是 Baseline() 也用同一份 CREATE SQL,于是用本工具建出来的表会
//     **没有主键**,而 player_database 的存盘走 INSERT ... ON DUPLICATE KEY
//     UPDATE —— 没有主键就没有"重复"可言,每次存盘都会追加一行新记录,
//     玩家数据表会无限膨胀且读出来的是任意一行。
//
// 所以这里直接读 proto 的 OptionPrimaryKey 扩展(它才是人写下的意图),
// 建表 SQL 只作为回退。
func declaredPrimaryKey(model *proto2mysql.PbMysqlDB, msg proto.Message) string {
	if pk := primaryKeyFromOptions(msg); pk != "" {
		return pk
	}
	m := primaryKeyRe.FindStringSubmatch(model.GetCreateTableSQL(msg))
	if len(m) != 2 {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(m[1]), "`", "")
}

// primaryKeyFromOptions 从 message 的 proto 选项里解出主键。
//
// 两条来源,优先级从高到低:
//  1. OptionPrimaryKey —— 显式声明,可以是 "user_id,provider" 这样的复合键;
//  2. OptionIsPlayerDatabase —— 玩家分表约定,主键恒为 player_id。
//     player_database / player_database_1 只标了这个选项、没标 OptionPrimaryKey,
//     但 mysql_database_table.sql 里它们确实是 PRIMARY KEY (player_id)。
//
// 两者都没有时返回空串,表示"proto 没表达过主键意图",调用方不产生告警
// (避免对 KV 型辅助表误报)。
func primaryKeyFromOptions(msg proto.Message) string {
	if msg == nil {
		return ""
	}
	opts, ok := msg.ProtoReflect().Descriptor().Options().(*descriptorpb.MessageOptions)
	if !ok || opts == nil {
		return ""
	}
	if v, ok := proto.GetExtension(opts, dbpb.E_OptionPrimaryKey).(string); ok {
		if pk := normalizeKeyList(v); pk != "" {
			return pk
		}
	}
	if v, ok := proto.GetExtension(opts, dbpb.E_OptionIsPlayerDatabase).(bool); ok && v {
		return "player_id"
	}
	return ""
}

// ensurePrimaryKey 在建表语句缺 PRIMARY KEY 时把 proto 声明的主键补进去。
//
// 为什么必须补而不是只告警:proto2mysql v0.0.18 的 GetCreateTableSQL 对
// player_database / player_database_1 不输出 PRIMARY KEY 子句。若原样拿它建表,
// 建出来的玩家数据表**没有主键**,而存盘走的是 INSERT ... ON DUPLICATE KEY
// UPDATE —— 没有唯一键就永远不会命中 "duplicate",每次存盘都追加一行新记录:
// 表无限膨胀,读回来的是任意一行,等于玩家数据静默损坏。
// go/db/model/mysql_database_table.sql 里这两张表写的是 PRIMARY KEY (player_id),
// 本函数保证按 proto 建出来的表与那份 schema 一致。
//
// pk 为空(proto 没表达过主键意图)时原样返回,不给 KV 型辅助表硬塞主键。
func ensurePrimaryKey(ddl, pk, table string) (string, error) {
	if pk == "" || primaryKeyRe.MatchString(ddl) {
		return ddl, nil
	}
	// 建表语句形如 "CREATE TABLE ... (\n  col ...,\n  col ...\n) ENGINE=..."。
	// 在最后一个右括号前插入 PRIMARY KEY 子句。
	end := strings.LastIndex(ddl, ")")
	if end < 0 {
		return "", fmt.Errorf("table %s: cannot locate column list in generated DDL, refusing to emit a primary-key-less CREATE TABLE", table)
	}
	cols := make([]string, 0, 2)
	for _, c := range strings.Split(pk, ",") {
		cols = append(cols, quoteIdent(c))
	}
	head := strings.TrimRight(ddl[:end], " \t\r\n")
	return head + ",\n  PRIMARY KEY (" + strings.Join(cols, ", ") + ")\n" + ddl[end:], nil
}

// normalizeKeyList 把 "user_id, provider" 规整成 "user_id,provider"。
func normalizeKeyList(raw string) string {
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(strings.ReplaceAll(p, "`", "")); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// LoadColumns 读出目标库里每张表的真实列类型(COLUMN_TYPE,含长度与 unsigned)。
func LoadColumns(ctx context.Context, q Queryer, database string) (map[string]map[string]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ?",
		database)
	if err != nil {
		return nil, fmt.Errorf("load columns of %s: %w", database, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]map[string]string)
	for rows.Next() {
		var table, column, columnType string
		if err := rows.Scan(&table, &column, &columnType); err != nil {
			return nil, fmt.Errorf("scan information_schema.COLUMNS: %w", err)
		}
		if out[table] == nil {
			out[table] = make(map[string]string)
		}
		out[table][column] = columnType
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate information_schema.COLUMNS: %w", err)
	}
	return out, nil
}

// LoadPrimaryKeyTables 返回目标库里**有主键**的表名集合。
func LoadPrimaryKeyTables(ctx context.Context, q Queryer, database string) (map[string]struct{}, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT DISTINCT TABLE_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME = 'PRIMARY'",
		database)
	if err != nil {
		return nil, fmt.Errorf("load primary keys of %s: %w", database, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]struct{})
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan information_schema.STATISTICS: %w", err)
		}
		out[table] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate information_schema.STATISTICS: %w", err)
	}
	return out, nil
}

// TypesCompatible 判断库里的真实列类型是否已经满足 proto 声明。
//
// 只比「基础类型 + unsigned」,不比长度与默认值:长度差异(varchar(191) vs
// varchar(255))在这里报出来只会制造噪音,而真正危险的是基础类型换了族
// (varchar → mediumtext 那种,会把主键做不了)。
func TypesCompatible(current, target string) bool {
	c := parseColumnType(current)
	t := parseColumnType(target)
	if c.base != t.base {
		return false
	}
	return c.unsigned == t.unsigned
}

type columnType struct {
	base     string
	unsigned bool
}

// parseColumnType 把 "bigint unsigned NOT NULL DEFAULT 0" / "tinyint(1)"
// 之类的串归一成 {base, unsigned}。
func parseColumnType(raw string) columnType {
	s := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.Index(s, " default "); idx >= 0 {
		s = s[:idx]
	}
	for _, kw := range []string{" not null", " null", " auto_increment", " zerofill"} {
		s = strings.ReplaceAll(s, kw, "")
	}
	// 去掉长度 / 精度括号,包括 enum('a','b') 这种带逗号的。
	if open := strings.IndexByte(s, '('); open >= 0 {
		if close := strings.LastIndexByte(s, ')'); close > open {
			s = s[:open] + s[close+1:]
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

// quoteIdent 反引号转义标识符。库名 / 表名 / 列名都来自 proto 与配置,
// 不来自玩家输入,但 DDL 无法参数化,统一转义省得留口子。
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// checksum 对语句集合算 sha256,用于漂移迁移的幂等去重。
func checksum(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
