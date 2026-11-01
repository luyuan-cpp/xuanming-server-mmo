package main

// 一致快照上的逐表校验和。
//
// TiDB 不直接比 ADMIN CHECKSUM TABLE 的 Checksum_crc64_xor:它把 table_id 算进去,两个集群上同样的数据校验和不同
// (官方文档原话,BR 和 TiCDC 建出来的表 table_id 都和源不同)。所以 TiDB 档比三样:
//   - ADMIN CHECKSUM TABLE 的 Total_kvs:行 KV 加索引 KV 的条数,和 table_id 无关,能发现索引和行对不上;
//   - 行数;
//   - 逐行内容摘要 BIT_XOR(MD5(CONCAT_WS(...))),和 sync-diff-inspector 同一个做法。
//
// 都在 tidb_snapshot 固定的快照上读。MySQL 档用 CHECKSUM TABLE 和行数:它要读锁,所以 verify 在复制副本上做(手册 §4)。

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"shared/authoritysql"
)

// tableDigest 是一张表的校验结果,两边逐字段相等才算一致。
type tableDigest struct {
	Columns string // 列名,按定义顺序
	Rows    uint64
	KVs     uint64 // ADMIN CHECKSUM TABLE 的 Total_kvs,MySQL 档是 0
	Digest  string // TiDB:逐行摘要;MySQL:CHECKSUM TABLE
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
			mismatches = append(mismatches, fmt.Sprintf("%s: source rows=%d kvs=%d digest=%s, target rows=%d kvs=%d digest=%s", table, a.Rows, a.KVs, a.Digest, b.Rows, b.KVs, b.Digest))
			continue
		}
		logf("%s: %d rows match", table, a.Rows)
	}
	for table := range onTarget {
		mismatches = append(mismatches, table+": exists only on the target")
	}
	return mismatches, nil
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

func (v *sqlView) Tables(ctx context.Context) ([]string, error) {
	return v.names(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=? AND table_type='BASE TABLE' ORDER BY table_name", v.schema)
}

func (v *sqlView) names(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := v.conn.QueryContext(ctx, query, args...)
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

func (v *sqlView) TableDigest(ctx context.Context, table string) (tableDigest, error) {
	columns, err := v.names(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema=? AND table_name=? ORDER BY ordinal_position", v.schema, table)
	if err != nil {
		return tableDigest{}, err
	}
	if len(columns) == 0 {
		return tableDigest{}, fmt.Errorf("table %s has no columns", table)
	}
	digest := tableDigest{Columns: strings.Join(columns, ",")}
	full := quoteIdentifier(v.schema) + "." + quoteIdentifier(table)
	if v.backend == authoritysql.BackendTiDB {
		var dbName, tableName, crc string
		var totalBytes uint64
		// Checksum_crc64_xor 读出来不比,理由见文件头。
		if err := v.conn.QueryRowContext(ctx, "ADMIN CHECKSUM TABLE "+full).Scan(&dbName, &tableName, &crc, &digest.KVs, &totalBytes); err != nil {
			return tableDigest{}, fmt.Errorf("ADMIN CHECKSUM TABLE: %w", err)
		}
		var xor uint64
		if err := v.conn.QueryRowContext(ctx, tidbRowDigestSQL(full, columns)).Scan(&digest.Rows, &xor); err != nil {
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

// tidbRowDigestSQL 拼逐行摘要。CONCAT_WS 跳过 NULL,所以末尾补一段 ISNULL 位图,NULL 和空串才算不同。
// 取 MD5 的高 64 位做 BIT_XOR:和行序无关,两个集群的扫描顺序不用相同。
func tidbRowDigestSQL(full string, columns []string) string {
	quoted := make([]string, len(columns))
	nulls := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdentifier(column)
		nulls[i] = "ISNULL(" + quoted[i] + ")"
	}
	row := "CONCAT_WS(','," + strings.Join(quoted, ",") + ",CONCAT(" + strings.Join(nulls, ",") + "))"
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
