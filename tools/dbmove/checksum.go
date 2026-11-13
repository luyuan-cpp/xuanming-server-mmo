package main

// 一致快照上的逐表校验和。每张表比两样:表的定义和表里的数据。
//
// 定义(structureText):每列的类型、排序规则、可空性、缺省值、生成方式,主键和每个索引的列序、前缀长度、唯一性。
// 只比数据不够:目标少一个唯一约束、列类型或排序规则不同时,数据摘要照样相同,翻转之后的行为却不一样。
// 源和目标是同一种后端(plan 核),所以两边的 information_schema 逐字比,不做归一化:
// 同一份定义在两个版本上展示不同(比如整数的显示宽度)也算不一致,要先把两边对齐。
// 表选项、注释、CHECK、外键、分区不在里面。
//
// 没有主键的表不比,直接拒绝:逐行摘要用 BIT_XOR 合并,两行完全相同就互相抵消,有主键才不会有这样的两行。
//
// 数据,TiDB 档不直接比 ADMIN CHECKSUM TABLE 的 Checksum_crc64_xor:它把 table_id 算进去,两个集群上同样的数据校验和不同
// (官方文档原话,BR 和 TiCDC 建出来的表 table_id 都和源不同)。所以比三样:
//   - ADMIN CHECKSUM TABLE 的 Total_kvs:行 KV 加索引 KV 的条数,和 table_id 无关,能发现索引和行对不上;
//   - 行数;
//   - 逐行内容摘要(tidbRowDigestSQL)。
//
// 都在 tidb_snapshot 固定的快照上读。MySQL 档用 CHECKSUM TABLE 和行数:它要读锁,所以 verify 在复制副本上做(手册 §4);
// 切换窗口里的那一次,源库冻结着、目标还没开放,两边直接在主库上比。

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"shared/authoritysql"
)

// tableDigest 是一张表的校验结果,两边逐字段相等才算一致。
type tableDigest struct {
	Structure string // 表的定义,见 structureText
	Rows      uint64
	KVs       uint64 // ADMIN CHECKSUM TABLE 的 Total_kvs,MySQL 档是 0
	Digest    string // TiDB:逐行摘要;MySQL:CHECKSUM TABLE
}

// checksumView 是一侧固定位置上的只读视图。
type checksumView interface {
	Tables(ctx context.Context) ([]string, error)
	TableDigest(ctx context.Context, table string) (tableDigest, error)
	// CheckPoint 核这个视图真的站在 cutover 记下的目标位置上(audit 用)。TiDB 靠 tidb_snapshot,没什么要核;MySQL 档核副本的 GTID。
	CheckPoint(ctx context.Context, point string) error
	// GateRows 读闸门的两张表,audit 单独比(见 compareGateRows)。
	GateRows(ctx context.Context) (gateRows, error)
	Close() error
}

// gateMetaRow、gateSlotRow 是闸门两张表里 audit 要比的列。
type gateMetaRow struct {
	Generation      string
	Writable        uint32
	WriterEpoch     uint64
	CatalogRevision uint64
	FreezeRunID     string
}

type gateSlotRow struct {
	WriterKey   string
	Slot        uint32
	Generation  string
	WriterEpoch uint64
	State       uint32
	LockTouch   uint32
}

type gateRows struct {
	Meta  gateMetaRow
	Slots []gateSlotRow // 按 (writer_key,slot) 排序
}

// auditSeparateTables 是 audit 不按校验和比的表。
//   - route_meta、writer_slot:打开目标闸门的那一次 release 只改它们,比法见 compareGateRows。
//   - db_move_sentinel:只有工具写。窗口崩了重跑时源库多一行新哨兵,而 audit 站在第一次记下的位置上,这一行差异和业务数据无关。
var auditSeparateTables = map[string]bool{"route_meta": true, "writer_slot": true, authoritysql.MoveSentinelTable: true}

// compareGateRows 比闸门表,允许目标正好多一次 release:源库冻结在 epoch E,目标 epoch E+1、已打开,ACTIVE 的 slot 跟着 E+1,其余一字不差。
// 目标站在记下的位置上时两边相同。多于一次 release 或者别的差异都算不一致。
func compareGateRows(source, target gateRows) []string {
	var problems []string
	delta := uint64(0)
	if target.Meta != source.Meta {
		released := source.Meta
		released.Writable = 1
		released.WriterEpoch++
		released.FreezeRunID = ""
		if target.Meta != released {
			return []string{fmt.Sprintf("route_meta: source %+v, target %+v: differs by more than the one release that opened the target gate", source.Meta, target.Meta)}
		}
		delta = 1
	}
	if len(source.Slots) != len(target.Slots) {
		return []string{fmt.Sprintf("writer_slot: source has %d rows, target has %d", len(source.Slots), len(target.Slots))}
	}
	for i, want := range source.Slots {
		if want.State == 1 {
			want.WriterEpoch += delta
		}
		if target.Slots[i] != want {
			problems = append(problems, fmt.Sprintf("writer_slot: writer %s slot %d: source (with this release) %+v, target %+v", want.WriterKey, want.Slot, want, target.Slots[i]))
		}
	}
	return problems
}

// compareViews 比两侧全部表,返回不一致的描述。读失败返回错误:没比完不算一致。
func compareViews(ctx context.Context, source, target checksumView, logf func(string, ...any)) ([]string, error) {
	return compareViewsExcept(ctx, source, target, nil, logf)
}

// compareViewsExcept 同 compareViews,跳过 skip 里的表。
func compareViewsExcept(ctx context.Context, source, target checksumView, skip map[string]bool, logf func(string, ...any)) ([]string, error) {
	sourceTables, err := source.Tables(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source tables: %w", err)
	}
	targetTables, err := target.Tables(ctx)
	if err != nil {
		return nil, fmt.Errorf("list target tables: %w", err)
	}
	if len(sourceTables) == 0 {
		return nil, errors.New("the source has no tables: wrong database?")
	}
	onTarget := make(map[string]bool, len(targetTables))
	for _, table := range targetTables {
		if !skip[table] {
			onTarget[table] = true
		}
	}
	var mismatches []string
	for _, table := range sourceTables {
		if skip[table] {
			continue
		}
		if !onTarget[table] {
			mismatches = append(mismatches, table+": missing on the target")
			continue
		}
		delete(onTarget, table)
		a, err := source.TableDigest(ctx, table)
		if err != nil {
			return nil, fmt.Errorf("checksum %s on the source: %w", table, err)
		}
		b, err := target.TableDigest(ctx, table)
		if err != nil {
			return nil, fmt.Errorf("checksum %s on the target: %w", table, err)
		}
		if a != b {
			mismatches = append(mismatches, table+": "+describeDifference(a, b))
			continue
		}
		logf("%s: %d rows match", table, a.Rows)
	}
	for table := range onTarget {
		mismatches = append(mismatches, table+": exists only on the target")
	}
	return mismatches, nil
}

// describeDifference 说明一张表两边差在哪。定义不同时只报定义:列和索引对不上,数据摘要本来就不可比。
func describeDifference(source, target tableDigest) string {
	if source.Structure != target.Structure {
		return fmt.Sprintf("the table definition differs: only on the source [%s], only on the target [%s]",
			strings.Join(linesMissingFrom(source.Structure, target.Structure), "; "), strings.Join(linesMissingFrom(target.Structure, source.Structure), "; "))
	}
	return fmt.Sprintf("source rows=%d kvs=%d digest=%s, target rows=%d kvs=%d digest=%s", source.Rows, source.KVs, source.Digest, target.Rows, target.KVs, target.Digest)
}

// linesMissingFrom 列出 text 里有、other 里没有的行。
func linesMissingFrom(text, other string) []string {
	have := map[string]bool{}
	for _, line := range strings.Split(other, "\n") {
		have[line] = true
	}
	var missing []string
	for _, line := range strings.Split(text, "\n") {
		if !have[line] {
			missing = append(missing, line)
		}
	}
	return missing
}

// primaryIndexName 是主键在 information_schema.statistics 里的索引名,MySQL 和 TiDB 相同。
const primaryIndexName = "PRIMARY"

// columnDef 是表定义里的一列,取自 information_schema.columns。
type columnDef struct {
	Name      string
	Type      string         // COLUMN_TYPE:含长度、精度、unsigned、enum 的取值
	Collation string         // 字符列的排序规则,别的列为空
	Nullable  string         // YES 或 NO
	Default   sql.NullString // 缺省值,没有时无效
	Extra     string         // auto_increment、on update、生成列的种类
	Generated string         // 生成列的表达式
}

// indexPart 是索引里的一列,取自 information_schema.statistics。
type indexPart struct {
	Index      string
	Unique     bool
	Type       string // BTREE、HASH、FULLTEXT 等
	Column     string
	Expression string // 表达式索引的这一部分;这时 Column 不是真实的列
	Prefix     string // 前缀长度,整列为空
	Order      string // A 升序,D 降序
}

func hasPrimaryKey(parts []indexPart) bool {
	return slices.ContainsFunc(parts, func(part indexPart) bool { return part.Index == primaryIndexName })
}

// noPrimaryKeyError 是表没有主键时的拒绝。plan 在开始搬之前就按同一个判据拦(checkNewMove),这里兜住之后才出现的表。
func noPrimaryKeyError(table string) error {
	return refusedf("table %s has no primary key: two identical rows cancel out in the row digest, so a difference could go unnoticed. Give it a primary key before moving", table)
}

// structureText 把表的定义写成逐行的文本:列按定义顺序,索引按名字,索引里的列按 parts 给的先后(调用方按 SEQ_IN_INDEX 排好)。
// 值都加引号转义,里面有换行、逗号也不会串行。两边的文本一字不差才算定义相同。
func structureText(columns []columnDef, parts []indexPart) string {
	var lines []string
	for i, column := range columns {
		defaultValue := "none"
		if column.Default.Valid {
			defaultValue = strconv.Quote(column.Default.String)
		}
		lines = append(lines, fmt.Sprintf("column %d %q type=%q collation=%q nullable=%s default=%s extra=%q generated=%q",
			i+1, column.Name, column.Type, column.Collation, column.Nullable, defaultValue, column.Extra, column.Generated))
	}
	// 按字节序排索引名:不依赖服务器 ORDER BY 用的排序规则。稳定排序保住同一个索引里列的先后。
	sorted := slices.Clone(parts)
	slices.SortStableFunc(sorted, func(a, b indexPart) int { return strings.Compare(a.Index, b.Index) })
	for start := 0; start < len(sorted); {
		first := sorted[start]
		var keyParts []string
		end := start
		for ; end < len(sorted) && sorted[end].Index == first.Index; end++ {
			part := sorted[end]
			keyParts = append(keyParts, fmt.Sprintf("%q expression=%q prefix=%q order=%q", part.Column, part.Expression, part.Prefix, part.Order))
		}
		kind := "index"
		switch {
		case first.Index == primaryIndexName:
			kind = "primary key"
		case first.Unique:
			kind = "unique index"
		}
		lines = append(lines, fmt.Sprintf("%s %q type=%q (%s)", kind, first.Index, first.Type, strings.Join(keyParts, ", ")))
		start = end
	}
	return strings.Join(lines, "\n")
}

// sqlView 实现 checksumView。一条连接从头用到尾:tidb_snapshot 和 time_zone 是会话变量。
type sqlView struct {
	db      *sql.DB
	conn    *sql.Conn
	schema  string
	backend authoritysql.Backend
}

func openChecksumView(ctx context.Context, rawDSN, schema string, backend authoritysql.Backend, snapshot string) (*sqlView, error) {
	db, err := openRaw(rawDSN, 1)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	view := &sqlView{db: db, conn: conn, schema: schema, backend: backend}
	// TIMESTAMP 列按会话时区转成文本,两边要相同。
	statements := []string{"SET SESSION time_zone='+00:00'"}
	if backend == authoritysql.BackendTiDB && snapshot != "" {
		ts, err := strconv.ParseUint(snapshot, 10, 64)
		if err != nil {
			_ = view.Close()
			return nil, fmt.Errorf("bad snapshot %q", snapshot)
		}
		statements = append(statements, fmt.Sprintf("SET SESSION tidb_snapshot='%d'", ts))
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			_ = view.Close()
			return nil, fmt.Errorf("%s: %w", statement, err)
		}
	}
	return view, nil
}

func (v *sqlView) Close() error {
	return errors.Join(v.conn.Close(), v.db.Close())
}

// baseTablesSQL 列一个库的全部业务表。视图不算:它没有自己的数据。
const baseTablesSQL = "SELECT table_name FROM information_schema.tables WHERE table_schema=? AND table_type='BASE TABLE' ORDER BY table_name"

func (v *sqlView) Tables(ctx context.Context) ([]string, error) {
	return queryNames(ctx, v.conn, baseTablesSQL, v.schema)
}

// rowQuerier 是 *sql.DB 和 *sql.Conn 都有的查询入口。
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryNames 读只有一列名字的结果集。
func queryNames(ctx context.Context, db rowQuerier, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// columns 读一张表的列,按定义顺序。
func (v *sqlView) columns(ctx context.Context, table string) ([]columnDef, error) {
	rows, err := v.conn.QueryContext(ctx, "SELECT column_name,column_type,collation_name,is_nullable,column_default,extra,generation_expression FROM information_schema.columns WHERE table_schema=? AND table_name=? ORDER BY ordinal_position", v.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []columnDef
	for rows.Next() {
		var column columnDef
		var collation, extra, generated sql.NullString
		if err := rows.Scan(&column.Name, &column.Type, &collation, &column.Nullable, &column.Default, &extra, &generated); err != nil {
			return nil, err
		}
		column.Collation, column.Extra, column.Generated = collation.String, extra.String, generated.String
		out = append(out, column)
	}
	return out, rows.Err()
}

// indexParts 读一张表全部索引的列,同一个索引里按 SEQ_IN_INDEX 排。主键也在里面,索引名是 PRIMARY。
// NON_UNIQUE 在 MySQL 是整数、在 TiDB 是字符串,读成文本两边都是 "0" 和 "1"。
func (v *sqlView) indexParts(ctx context.Context, table string) ([]indexPart, error) {
	rows, err := v.conn.QueryContext(ctx, "SELECT index_name,non_unique,index_type,column_name,`expression`,sub_part,`collation` FROM information_schema.statistics WHERE table_schema=? AND table_name=? ORDER BY index_name,seq_in_index", v.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indexPart
	for rows.Next() {
		var part indexPart
		var nonUnique string
		var indexType, column, expression, prefix, order sql.NullString
		if err := rows.Scan(&part.Index, &nonUnique, &indexType, &column, &expression, &prefix, &order); err != nil {
			return nil, err
		}
		part.Unique = nonUnique == "0"
		part.Type, part.Column, part.Expression, part.Prefix, part.Order = indexType.String, column.String, expression.String, prefix.String, order.String
		out = append(out, part)
	}
	return out, rows.Err()
}

func (v *sqlView) TableDigest(ctx context.Context, table string) (tableDigest, error) {
	columns, err := v.columns(ctx, table)
	if err != nil {
		return tableDigest{}, fmt.Errorf("read the columns: %w", err)
	}
	if len(columns) == 0 {
		return tableDigest{}, fmt.Errorf("table %s has no columns", table)
	}
	parts, err := v.indexParts(ctx, table)
	if err != nil {
		return tableDigest{}, fmt.Errorf("read the indexes: %w", err)
	}
	// 排在全表扫描前面:没有主键的表摘要不可信,不值得扫。
	if !hasPrimaryKey(parts) {
		return tableDigest{}, noPrimaryKeyError(table)
	}
	digest := tableDigest{Structure: structureText(columns, parts)}
	full := quoteIdentifier(v.schema) + "." + quoteIdentifier(table)
	if v.backend == authoritysql.BackendTiDB {
		var dbName, tableName, crc string
		var totalBytes uint64
		// Checksum_crc64_xor 读出来不比,理由见文件头。
		if err := v.conn.QueryRowContext(ctx, "ADMIN CHECKSUM TABLE "+full).Scan(&dbName, &tableName, &crc, &digest.KVs, &totalBytes); err != nil {
			return tableDigest{}, fmt.Errorf("ADMIN CHECKSUM TABLE: %w", err)
		}
		names := make([]string, len(columns))
		for i, column := range columns {
			names[i] = column.Name
		}
		var xor uint64
		if err := v.conn.QueryRowContext(ctx, tidbRowDigestSQL(full, names)).Scan(&digest.Rows, &xor); err != nil {
			return tableDigest{}, fmt.Errorf("row digest: %w", err)
		}
		digest.Digest = strconv.FormatUint(xor, 16)
		return digest, nil
	}
	var name string
	var checksum sql.NullString
	if err := v.conn.QueryRowContext(ctx, "CHECKSUM TABLE "+full).Scan(&name, &checksum); err != nil {
		return tableDigest{}, fmt.Errorf("CHECKSUM TABLE: %w", err)
	}
	if !checksum.Valid {
		return tableDigest{}, errors.New("CHECKSUM TABLE returned NULL: the table does not exist")
	}
	digest.Digest = checksum.String
	if err := v.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+full).Scan(&digest.Rows); err != nil {
		return tableDigest{}, fmt.Errorf("count rows: %w", err)
	}
	return digest, nil
}

// rowDigestNull 顶替 NULL 列的 MD5。它不是十六进制字符,和任何一个 MD5 都不会混。
const rowDigestNull = "N"

// tidbRowDigestSQL 拼逐行摘要:每列先各取 MD5,NULL 用 rowDigestNull 顶替,拼起来再取一次 MD5。
// 每列要么是定长的 32 个十六进制字符,要么是那一个顶替字符,所以一行只有一种拼法:值里有逗号、是空串还是 NULL 都分得清。
// 把各列的值用分隔符直接连起来做不到这一点('a,b' 和 'c' 连出来,与 'a' 和 'b,c' 连出来是同一串)。
// 取 MD5 的高 64 位做 BIT_XOR:和行序无关,两个集群的扫描顺序不用相同。表都有主键,没有完全相同的两行,不会成对抵消。
func tidbRowDigestSQL(full string, columns []string) string {
	fields := make([]string, len(columns))
	for i, column := range columns {
		fields[i] = "IFNULL(MD5(" + quoteIdentifier(column) + "),'" + rowDigestNull + "')"
	}
	row := "CONCAT(" + strings.Join(fields, ",") + ")"
	return "SELECT COUNT(*),BIT_XOR(CAST(CONV(SUBSTRING(MD5(" + row + "),1,16),16,10) AS UNSIGNED)) FROM " + full
}

// GateRows 见 checksumView。和校验和同一条连接,站在同一个快照上。
func (v *sqlView) GateRows(ctx context.Context) (gateRows, error) {
	schema := quoteIdentifier(v.schema)
	var out gateRows
	var freezeRun []byte
	err := v.conn.QueryRowContext(ctx, "SELECT generation,writable,writer_epoch,catalog_revision,freeze_run_id FROM "+schema+".route_meta WHERE id=1").
		Scan(&out.Meta.Generation, &out.Meta.Writable, &out.Meta.WriterEpoch, &out.Meta.CatalogRevision, &freezeRun)
	if err != nil {
		return gateRows{}, fmt.Errorf("read route_meta: %w", err)
	}
	out.Meta.FreezeRunID = hex.EncodeToString(freezeRun)
	rows, err := v.conn.QueryContext(ctx, "SELECT writer_key,slot,generation,writer_epoch,state,lock_touch FROM "+schema+".writer_slot ORDER BY writer_key,slot")
	if err != nil {
		return gateRows{}, fmt.Errorf("read writer_slot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slot gateSlotRow
		var key []byte
		if err := rows.Scan(&key, &slot.Slot, &slot.Generation, &slot.WriterEpoch, &slot.State, &slot.LockTouch); err != nil {
			return gateRows{}, fmt.Errorf("read writer_slot: %w", err)
		}
		slot.WriterKey = hex.EncodeToString(key)
		out.Slots = append(out.Slots, slot)
	}
	if err := rows.Err(); err != nil {
		return gateRows{}, fmt.Errorf("read writer_slot: %w", err)
	}
	return out, nil
}

// CheckPoint 见 checksumView。MySQL 档:这个副本必须正好停在记下的 GTID 集合上,多一个事务就可能含翻转之后的写入。
func (v *sqlView) CheckPoint(ctx context.Context, point string) error {
	if v.backend == authoritysql.BackendTiDB {
		return nil
	}
	var behind, ahead int
	err := v.conn.QueryRowContext(ctx, "SELECT GTID_SUBSET(?,@@global.gtid_executed),GTID_SUBSET(@@global.gtid_executed,?)", point, point).Scan(&behind, &ahead)
	if err != nil {
		return fmt.Errorf("read the replica position: %w", err)
	}
	if behind != 1 || ahead != 1 {
		return refusedf("the audit replica is not at the recorded target point %s (reached it: %t, went past it: %t): position it as in docs/ops/db-move-runbook.md §8", point, behind == 1, ahead != 1)
	}
	return nil
}
