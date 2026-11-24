// dbcheck — 发布/压测前置只读数据库容量、索引与大字段体检工具（移植自 xuanming-server/tools/migrate/cmd/dbcheck）。
//
// 职责：
//  1. 广度体检：枚举目标实例上的 mmorpg* 逻辑库（可用 -schema-like 指定），核对表登记清单、
//     清理路径必需索引的前导列顺序、以及 outbox/待投递队列精确行数上限（-outbox-max）；
//  2. 压测快照对比：-snapshot before.json / -compare before.json 输出逐表增量，核对增长是否与业务量匹配；
//  3. 深度体检（-size-check [-top-rows N]）：同时检查 AVG_ROW_LENGTH 与核心 BLOB/MEDIUMBLOB/VARBINARY 列
//     的 MAX/AVG 字节数和超预算行数，避免单行变胖引发写入失败；
//  4. 待清理量上界报告（-pending）：仅执行 SELECT COUNT(*)，永不执行 DELETE/UPDATE/TRUNCATE
//     （遵守 docs/rules/engineering-principles.md 容量只报告不自动删数红线）。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type tableClass string

const (
	classBounded tableClass = "bounded"
	classSwept   tableClass = "swept"
	classOutbox  tableClass = "outbox"
	classExempt  tableClass = "exempt"
)

// indexSpec 描述清理路径必需索引的名称与前导列顺序（严格前缀匹配）。
type indexSpec struct {
	Name    string
	Columns []string
}

type tableEntry struct {
	Class           tableClass
	RequiredIndexes []indexSpec
	PendingWhere    string // 仅用于 -pending 的只读 COUNT(*)，永不用于 DELETE
}

// ignoredTables 是各域逻辑库共用的框架与权威元数据表，不计入业务未登记表。
var ignoredTables = map[string]bool{
	"schema_migrations":              true,
	"schemamigrate_history":          true,
	"db_move_sentinel":               true,
	"route_meta":                     true,
	"writer_registry":                true,
	"writer_slot":                    true,
	"writer_lifecycle_receipt":       true,
	"sql_writer_drain_run":           true,
	"sql_writer_freeze_ticket":       true,
	"sql_writer_maintenance_receipt": true,
	"role_fence":                     true,
}

// defaultRegistry 记录各核心域库的表分类与索引基线；支持按实际部署库子集检查（多集群放置下各库可能分散在不同实例）。
var defaultRegistry = map[string]map[string]tableEntry{
	"mmorpg_player_inbox": {
		"player_inbox_stream": {Class: classBounded},
		"player_inbox_item":   {Class: classExempt}, // 终态行永久收据（设计 §9.1）
		"player_inbox_audit":  {Class: classExempt},
	},
	"mmorpg_chat": {
		"chat_send_authority":       {Class: classSwept, PendingWhere: "accepted_at < NOW()"},
		"chat_policy":               {Class: classBounded},
		"chat_world_retraction":     {Class: classSwept, PendingWhere: "accepted_at < NOW()"},
		"chat_report":               {Class: classSwept, PendingWhere: "accepted_at < NOW()"},
		"chat_repeat_event":         {Class: classSwept},
		"chat_private_daily_target": {Class: classSwept},
	},
	"mmorpg_friend": {
		"friend":         {Class: classBounded},
		"friend_request": {Class: classSwept},
	},
	"mmorpg_mail": {
		"mail_message": {Class: classSwept},
		"mail_state":   {Class: classSwept},
	},
	"mmorpg_guild": {
		"guild":        {Class: classBounded},
		"guild_member": {Class: classBounded},
	},
}

type snapshot map[string]int64

type options struct {
	schemaLike     string
	exact          bool
	strictRegistry bool
	snapshotOut    string
	compareWith    string
	outboxMax      int64
	avgRowWarn     int64
	pending        bool
	sizeCheck      bool
	topRows        int
}

func main() {
	dsn := flag.String("dsn", "", "MySQL/TiDB DSN without database (e.g. root:pwd@tcp(127.0.0.1:4000)/)")
	schemaLike := flag.String("schema-like", `mmorpg%`, "LIKE pattern for logical databases to inspect")
	exact := flag.Bool("exact", false, "use exact SELECT COUNT(*) for all tables (default: information_schema estimate except outbox)")
	strictRegistry := flag.Bool("strict-registry", false, "fail on tables present in known schemas but absent from registry")
	snapshotOut := flag.String("snapshot", "", "write row count snapshot to JSON file")
	compareWith := flag.String("compare", "", "compare current counts against a previous snapshot JSON file")
	outboxMax := flag.Int64("outbox-max", 200, "maximum allowed rows in any outbox table")
	avgRowWarn := flag.Int64("avg-row-warn", 64*1024, "warn when information_schema AVG_ROW_LENGTH exceeds this byte threshold")
	pending := flag.Bool("pending", false, "report read-only worst-case pending counts for swept tables (never deletes)")
	sizeCheck := flag.Bool("size-check", false, "run big-field MAX/AVG LENGTH scan on registered blob columns")
	topRows := flag.Int("top-rows", 0, "with -size-check, print top N largest primary keys for over-budget columns")
	timeout := flag.Duration("timeout", 15*time.Minute, "overall execution timeout")
	flag.Parse()

	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(os.Stderr, "usage: dbcheck -dsn 'user:pwd@tcp(host:port)/' [flags]")
		os.Exit(2)
	}
	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: open mysql failed: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: ping mysql failed: %v\n", err)
		os.Exit(2)
	}

	opts := options{
		schemaLike:     *schemaLike,
		exact:          *exact,
		strictRegistry: *strictRegistry,
		snapshotOut:    *snapshotOut,
		compareWith:    *compareWith,
		outboxMax:      *outboxMax,
		avgRowWarn:     *avgRowWarn,
		pending:        *pending,
		sizeCheck:      *sizeCheck,
		topRows:        *topRows,
	}
	violations, err := runCheck(ctx, db, defaultRegistry, defaultBigFields, opts, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(2)
	}
	if violations > 0 {
		fmt.Printf("\n[RESULT] FAIL: %d violation(s) found\n", violations)
		os.Exit(1)
	}
	fmt.Println("\n[RESULT] PASS: all read-only dbcheck gates succeeded")
}

type tableMeta struct {
	Name         string
	RowsEstimate int64
	AvgRowLength int64
}

func runCheck(
	ctx context.Context,
	db *sql.DB,
	reg map[string]map[string]tableEntry,
	fields []fieldBudget,
	opts options,
	out io.Writer,
) (int, error) {
	actual, err := listTables(ctx, db, opts.schemaLike)
	if err != nil {
		return 0, fmt.Errorf("list tables: %w", err)
	}
	if len(actual) == 0 {
		fmt.Fprintf(out, "[FAIL] no base tables matched schema pattern %q\n", opts.schemaLike)
		return 1, nil
	}

	violations := 0
	presentByDB := make(map[string]map[string]bool, len(actual))
	snap := snapshot{}

	dbNames := make([]string, 0, len(actual))
	for d := range actual {
		dbNames = append(dbNames, d)
	}
	sort.Strings(dbNames)

	for _, dbName := range dbNames {
		tables := actual[dbName]
		presentByDB[dbName] = make(map[string]bool, len(tables))
		dbReg, dbKnown := reg[dbName]
		for _, tm := range tables {
			presentByDB[dbName][tm.Name] = true
			if ignoredTables[tm.Name] {
				continue
			}
			entry, known := dbReg[tm.Name]
			if dbKnown && !known && opts.strictRegistry {
				fmt.Fprintf(out, "[FAIL] unregistered table: %s.%s\n", dbName, tm.Name)
				violations++
			}
			if opts.avgRowWarn > 0 && tm.AvgRowLength > opts.avgRowWarn {
				fmt.Fprintf(out, "[WARN] high AVG_ROW_LENGTH: %s.%s avg_row_bytes=%d threshold=%d\n",
					dbName, tm.Name, tm.AvgRowLength, opts.avgRowWarn)
			}

			// outbox 类表始终使用精确 COUNT(*)，防止统计滞后漏报积压。
			useExact := opts.exact || (known && entry.Class == classOutbox)
			n := tm.RowsEstimate
			if useExact {
				exactCount, cerr := exactRowCount(ctx, db, dbName, tm.Name)
				if cerr != nil {
					return violations, fmt.Errorf("count %s.%s: %w", dbName, tm.Name, cerr)
				}
				n = exactCount
			}
			snap[dbName+"."+tm.Name] = n

			if known && entry.Class == classOutbox && n > opts.outboxMax {
				fmt.Fprintf(out, "[FAIL] outbox backlog: %s.%s rows=%d max=%d\n", dbName, tm.Name, n, opts.outboxMax)
				violations++
			}
			if known && entry.Class == classSwept && len(entry.RequiredIndexes) > 0 {
				for _, idx := range entry.RequiredIndexes {
					ok, detail, ierr := indexMatches(ctx, db, dbName, tm.Name, idx)
					if ierr != nil {
						return violations, fmt.Errorf("probe index %s.%s.%s: %w", dbName, tm.Name, idx.Name, ierr)
					}
					if !ok {
						fmt.Fprintf(out, "[FAIL] missing/mismatched cleanup index %s.%s.%s (%s; want prefix %v)\n",
							dbName, tm.Name, idx.Name, detail, idx.Columns)
						violations++
					}
				}
			}
		}
	}

	if opts.snapshotOut != "" {
		b, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return violations, fmt.Errorf("marshal snapshot: %w", err)
		}
		if err := os.WriteFile(opts.snapshotOut, b, 0o644); err != nil {
			return violations, fmt.Errorf("write snapshot: %w", err)
		}
		fmt.Fprintf(out, "[OK] snapshot written to %s (%d tables)\n", opts.snapshotOut, len(snap))
	}

	if opts.compareWith != "" {
		if err := compareSnapshot(opts.compareWith, snap, out); err != nil {
			return violations, err
		}
	}

	if opts.sizeCheck {
		fmt.Fprintln(out, "\n-- Big-field bloat check (read-only) --")
		for _, s := range checkBigFields(ctx, db, fields, presentByDB) {
			target := s.budget.DB + "." + s.budget.Table + "." + s.budget.Column
			if s.err != nil {
				fmt.Fprintf(out, "  [WARN] %-52s probe failed: %v\n", target, s.err)
				continue
			}
			ratio := float64(s.maxBytes) / float64(max64(s.budget.MaxBytes, 1))
			mark := " OK "
			if s.overRows > 0 {
				mark = "FAIL"
				violations++
			} else if ratio >= 0.8 {
				mark = "WARN"
			}
			fmt.Fprintf(out, "  [%s] %-52s rows=%-9d max=%-9d avg=%-7d budget=%-8d over=%d\n",
				mark, target, s.rows, s.maxBytes, s.avgBytes, s.budget.MaxBytes, s.overRows)
			if opts.topRows > 0 && (s.overRows > 0 || ratio >= 0.8) {
				top, terr := topLargeRows(ctx, db, s.budget, opts.topRows)
				if terr != nil {
					fmt.Fprintf(out, "         [WARN] top rows query failed: %v\n", terr)
				}
				for _, r := range top {
					fmt.Fprintf(out, "         %s=%s bytes=%d\n", s.budget.PK, r.PK, r.Bytes)
				}
			}
		}
	}

	if opts.pending {
		fmt.Fprintln(out, "\n-- Read-only pending retention upper bound (no deletes) --")
		for _, dbName := range dbNames {
			dbReg, ok := reg[dbName]
			if !ok {
				continue
			}
			names := make([]string, 0, len(dbReg))
			for t := range dbReg {
				names = append(names, t)
			}
			sort.Strings(names)
			for _, t := range names {
				e := dbReg[t]
				if e.Class != classSwept || e.PendingWhere == "" || !presentByDB[dbName][t] {
					continue
				}
				var n int64
				q := "SELECT COUNT(*) FROM `" + dbName + "`.`" + t + "` WHERE " + e.PendingWhere
				if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
					fmt.Fprintf(out, "  [WARN] %-46s count failed: %v\n", dbName+"."+t, err)
					continue
				}
				fmt.Fprintf(out, "  %-46s pending_rows=%d\n", dbName+"."+t, n)
			}
		}
	}

	return violations, nil
}

func compareSnapshot(path string, current snapshot, out io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read compare snapshot: %w", err)
	}
	before := snapshot{}
	if err := json.Unmarshal(b, &before); err != nil {
		return fmt.Errorf("parse compare snapshot: %w", err)
	}
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(out, "-- Row count delta vs snapshot --")
	for _, k := range keys {
		delta := current[k] - before[k]
		if delta != 0 {
			fmt.Fprintf(out, "  %-50s %+d (%d -> %d)\n", k, delta, before[k], current[k])
		}
	}
	return nil
}

func listTables(ctx context.Context, db *sql.DB, schemaLike string) (map[string][]tableMeta, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT TABLE_SCHEMA, TABLE_NAME, COALESCE(TABLE_ROWS, 0), COALESCE(AVG_ROW_LENGTH, 0)
		 FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA LIKE ? AND TABLE_TYPE = 'BASE TABLE'
		 ORDER BY TABLE_SCHEMA, TABLE_NAME`, schemaLike)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]tableMeta{}
	for rows.Next() {
		var dbName string
		var tm tableMeta
		if err := rows.Scan(&dbName, &tm.Name, &tm.RowsEstimate, &tm.AvgRowLength); err != nil {
			return nil, err
		}
		out[dbName] = append(out[dbName], tm)
	}
	return out, rows.Err()
}

func exactRowCount(ctx context.Context, db *sql.DB, dbName, table string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+dbName+"`.`"+table+"`").Scan(&n)
	return n, err
}

func indexMatches(ctx context.Context, db *sql.DB, dbName, table string, spec indexSpec) (bool, string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?
		 ORDER BY SEQ_IN_INDEX`, dbName, table, spec.Name)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return false, "", err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if len(cols) == 0 {
		return false, "index not found", nil
	}
	if len(cols) < len(spec.Columns) {
		return false, fmt.Sprintf("actual columns %v", cols), nil
	}
	for i, want := range spec.Columns {
		if cols[i] != want {
			return false, fmt.Sprintf("actual columns %v", cols), nil
		}
	}
	return true, "", nil
}
