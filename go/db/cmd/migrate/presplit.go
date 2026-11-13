package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"db/internal/config"
	"db/internal/playerstore"
	"shared/authoritysql"
)

// TiDB 上的预切:建表之后把存档库里的两张写入热点表切成 tidbPresplitRegions 段,开服时的写入从第一刻起就分散在多个 Region 上,
// 不等 TiDB 按大小慢慢自动分裂(一个新表只有一个 Region,开服建角潮和存盘全落在一个 TiKV 上)。
//
//   - player_store(docs/design/player-save-direct-commit.md §1):主键 (player_id, kind) 聚簇;player_id 是发号器打散过的
//     63 位值(AGENTS.md §7 第 1 条),在 [0, 2^63) 上均匀,按值等分即可。
//   - player_audit_outbox(docs/design/player-audit-outbox.md §3):主键 (shard, kind, created_ms, player_id, record_id) 聚簇,
//     片号是第一列、取值 0..15:按片号切,每片的追加尾巴落在各自的 Region,转发按片分工时各扫各的。
//
// 聚簇表按主键值切,值要给全主键的每一列(TiDB 对 common handle 的 SPLIT … BY 要求列数等于主键列数),后几列取 0,即这一段的起点。
// 每次 up 都发:对已有边界再切不产生新 Region,幂等。MySQL 上什么都不做。
// 切不成只影响写入分散、不影响正确性:按 NEEDS-REVIEW 报(退出码 4),迁移本身照样算成功。
//
// 段数取 16:等于发件箱的片数;两张表在开服之后都会随数据量自动继续分裂,预切只解决起步的单 Region 热点。

const (
	// tidbPresplitRegions 是每张表预切出的段数,与发件箱片数相同。
	tidbPresplitRegions = playerstore.AuditOutboxShards
	// playerIDSpace 是发号器永久 ID 的取值上界(不含):63 位。
	playerIDSpace = uint64(1) << 63

	tidbPKTypeSQL = "SELECT TIDB_PK_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?"
)

// presplitTable 是一张要预切的表与它的 SPLIT 语句。
type presplitTable struct {
	name string
	stmt string
}

// presplitTables 是要预切的表。语句的值是确定的,单测核对。
func presplitTables() []presplitTable {
	return []presplitTable{
		{name: "player_store", stmt: splitStatement("player_store", playerStoreBounds())},
		{name: playerstore.AuditOutboxTable, stmt: splitStatement(playerstore.AuditOutboxTable, auditOutboxBounds())},
	}
}

// playerStoreBounds 把 [0, 2^63) 等分成 tidbPresplitRegions 段,边界写成 (player_id, kind=0)。
func playerStoreBounds() []string {
	step := playerIDSpace / tidbPresplitRegions
	bounds := make([]string, 0, tidbPresplitRegions-1)
	for i := uint64(1); i < tidbPresplitRegions; i++ {
		bounds = append(bounds, "("+strconv.FormatUint(i*step, 10)+", 0)")
	}
	return bounds
}

// auditOutboxBounds 按片号切,边界写成 (shard, 0, 0, 0, 0)。
func auditOutboxBounds() []string {
	bounds := make([]string, 0, playerstore.AuditOutboxShards-1)
	for shard := 1; shard < playerstore.AuditOutboxShards; shard++ {
		bounds = append(bounds, "("+strconv.Itoa(shard)+", 0, 0, 0, 0)")
	}
	return bounds
}

func splitStatement(table string, bounds []string) string {
	return "SPLIT TABLE `" + table + "` BY " + strings.Join(bounds, ", ")
}

// presplitTiDBTables 在 TiDB 后端上预切 presplitTables,返回要人工过目的告警。dryRun 只打印语句。
func presplitTiDBTables(ctx context.Context, db *sql.DB, database string, dryRun bool) []string {
	backend, err := authoritysql.ParseBackend(config.AppConfig.ServerConfig.Database.Backend)
	if err != nil {
		return []string{fmt.Sprintf("pre-split skipped: %v", err)}
	}
	if backend != authoritysql.BackendTiDB {
		return nil
	}
	var warnings []string
	for _, table := range presplitTables() {
		if dryRun {
			logf("PLAN %s (database=%s)", table.stmt, database)
			continue
		}
		if err := requireClustered(ctx, db, table.name); err != nil {
			warnings = append(warnings, fmt.Sprintf("pre-split of %s skipped on %s: %v", table.name, database, err))
			continue
		}
		regions, err := execSplit(ctx, db, table.stmt)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("pre-split of %s failed on %s (writes still work, only less spread): %v", table.name, database, err))
			continue
		}
		logf("DONE %s split_regions=%d (database=%s; 0 means the boundaries already existed)", table.stmt, regions, database)
	}
	return warnings
}

// requireClustered 核表在 TiDB 上是聚簇主键:按主键值切只对聚簇表有意义(非聚簇表的行按 _tidb_rowid 排),聚簇属性建表后改不了。
func requireClustered(ctx context.Context, db *sql.DB, table string) error {
	var pkType sql.NullString
	err := db.QueryRowContext(ctx, tidbPKTypeSQL, table).Scan(&pkType)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("table %s does not exist", table)
	}
	if err != nil {
		return fmt.Errorf("read TIDB_PK_TYPE of %s: %w", table, err)
	}
	if !strings.EqualFold(pkType.String, "CLUSTERED") {
		return fmt.Errorf("table %s has TIDB_PK_TYPE=%q, splitting by primary key values needs CLUSTERED (rebuild the table with tidb_enable_clustered_index=ON)",
			table, pkType.String)
	}
	return nil
}

// execSplit 执行 SPLIT 并取首行首列(TOTAL_SPLIT_REGION)。看不到结果就不能声称切过了,一律报错。
func execSplit(ctx context.Context, db *sql.DB, stmt string) (int64, error) {
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("read SPLIT result columns: %w", err)
	}
	if len(columns) == 0 || !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("SPLIT returned no result row")
	}
	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return 0, fmt.Errorf("scan SPLIT result: %w", err)
	}
	regions, err := strconv.ParseInt(values[0].String, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse TOTAL_SPLIT_REGION %q: %w", values[0].String, err)
	}
	return regions, rows.Err()
}
