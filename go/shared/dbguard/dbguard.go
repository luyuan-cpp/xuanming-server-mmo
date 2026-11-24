// Package dbguard 提供数据库容量与大字段运行时守卫（移植自 xuanming-server/pkg/dbguard）。
//
// 职责划分：
//   - 表级容量巡检（Guard.Check）：通过 information_schema.TABLES 采样估算 TABLE_ROWS /
//     DATA_LENGTH / AVG_ROW_LENGTH（毫秒级、不锁表），超预算仅打 ERROR 日志与 Prometheus 指标，
//     不阻断服务启动与运行；
//   - 列级大字段排查（CheckColumns / TopLargeRows）：全表扫描仅用于低频或人工排障；
//   - 写入侧三档字节守卫（CheckPayload）：达上限 fail-closed 拒写、达 80% 放行并打 WARN、
//     同时记录字节分布直方图；
//   - 保留期清理执行模式（SweepTable / ParseMode）：零值即 ModeReportOnly（默认只数不删），
//     COUNT 与 DELETE 共用同一 WHERE 条件防漂移。
package dbguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

var (
	metricsOnce sync.Once

	tableRows = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_table_rows",
		Help: "Estimated table row count from information_schema.TABLES.",
	}, []string{"db", "table"})

	tableRowsBudget = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_table_rows_budget",
		Help: "Configured row count budget for each table (0 = unset).",
	}, []string{"db", "table"})

	tableBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_table_bytes",
		Help: "Estimated table DATA_LENGTH in bytes from information_schema.TABLES.",
	}, []string{"db", "table"})

	tableAvgRowBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_avg_row_bytes",
		Help: "Estimated AVG_ROW_LENGTH in bytes; sudden jumps indicate unbounded blob/row growth.",
	}, []string{"db", "table"})

	budgetViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mmorpg_db_budget_violations_total",
		Help: "Count of table/column capacity budget violations by kind (rows|bytes|avg_row_bytes|column_bytes).",
	}, []string{"db", "table", "kind"})

	columnMaxBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_column_max_bytes",
		Help: "Maximum observed byte length of a guarded column (low-frequency scan).",
	}, []string{"db", "table", "column"})
)

func registerMetrics() {
	metricsOnce.Do(func() {
		collectors := []prometheus.Collector{
			tableRows,
			tableRowsBudget,
			tableBytes,
			tableAvgRowBytes,
			budgetViolations,
			columnMaxBytes,
			payloadBytes,
			payloadRejects,
			retentionPending,
			retentionDeleted,
		}
		for _, c := range collectors {
			if err := prometheus.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[dbguard] Failed to register prometheus collector: %v", err)
				}
			}
		}
	})
}

// TableBudget 描述单张表的容量预算；字段为 0 表示该维度不检查。
type TableBudget struct {
	Table          string
	MaxRows        int64
	MaxBytes       int64
	MaxAvgRowBytes int64
	Note           string
}

// ColumnBudget 描述单个大字段列的字节预算（全表扫描，仅低频或人工排障调用）。
type ColumnBudget struct {
	Table    string
	Column   string
	MaxBytes int64
	Note     string
}

// Guard 对应单个逻辑库 *sql.DB 的容量守卫实例。
type Guard struct {
	db      *sql.DB
	dbName  string
	tables  []TableBudget
	columns []ColumnBudget
}

// New 构造逻辑库容量守卫；dbName 为空时在首次 Check 自动查 SELECT DATABASE()。
func New(db *sql.DB, dbName string, tables []TableBudget, columns []ColumnBudget) *Guard {
	return &Guard{db: db, dbName: dbName, tables: tables, columns: columns}
}

// AssertStrictMode 断言当前连接的 session sql_mode 包含 STRICT_TRANS_TABLES。
// 非严格模式下超长写入会静默截断，属于无声数据损坏，调用方应在启动期 fail-fast 退出。
func AssertStrictMode(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, "SELECT @@session.sql_mode").Scan(&mode); err != nil {
		return fmt.Errorf("dbguard: query session sql_mode failed: %w", err)
	}
	for _, m := range strings.Split(mode, ",") {
		if strings.TrimSpace(m) == "STRICT_TRANS_TABLES" {
			return nil
		}
	}
	return fmt.Errorf("dbguard: session sql_mode %q missing STRICT_TRANS_TABLES", mode)
}

// Result 是一轮巡检的汇总结果。
type Result struct {
	Checked    int
	Violations []Violation
}

// Violation 描述一条超预算项。
type Violation struct {
	Table  string
	Column string
	Kind   string // rows | bytes | avg_row_bytes | column_bytes
	Actual int64
	Budget int64
	Note   string
}

// Check 执行一轮表级估算巡检（information_schema.TABLES，不锁表、不扫数据、永不阻断业务）。
func (g *Guard) Check(ctx context.Context) Result {
	registerMetrics()
	res := Result{}
	if g == nil || g.db == nil || len(g.tables) == 0 {
		return res
	}
	if g.dbName == "" {
		if err := g.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&g.dbName); err != nil || g.dbName == "" {
			logx.WithContext(ctx).Errorf("[dbguard] Failed to resolve active database name: %v", err)
			return res
		}
	}

	stats, err := g.tableStats(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("[dbguard] Failed to query table stats for db=%s: %v", g.dbName, err)
		return res
	}

	for _, b := range g.tables {
		s, ok := stats[b.Table]
		if !ok {
			continue
		}
		res.Checked++
		tableRows.WithLabelValues(g.dbName, b.Table).Set(float64(s.rows))
		tableBytes.WithLabelValues(g.dbName, b.Table).Set(float64(s.bytes))
		tableAvgRowBytes.WithLabelValues(g.dbName, b.Table).Set(float64(s.avgRow))
		tableRowsBudget.WithLabelValues(g.dbName, b.Table).Set(float64(b.MaxRows))

		check := func(kind string, actual, budget int64) {
			if budget <= 0 || actual <= budget {
				return
			}
			budgetViolations.WithLabelValues(g.dbName, b.Table, kind).Inc()
			res.Violations = append(res.Violations, Violation{
				Table: b.Table, Kind: kind, Actual: actual, Budget: budget, Note: b.Note,
			})
			logx.WithContext(ctx).Errorw("db_capacity_budget_exceeded",
				logx.Field("db", g.dbName),
				logx.Field("table", b.Table),
				logx.Field("kind", kind),
				logx.Field("actual", actual),
				logx.Field("budget", budget),
				logx.Field("note", b.Note),
			)
		}
		check("rows", s.rows, b.MaxRows)
		check("bytes", s.bytes, b.MaxBytes)
		check("avg_row_bytes", s.avgRow, b.MaxAvgRowBytes)
	}
	return res
}

type tableStat struct {
	rows   int64
	bytes  int64
	avgRow int64
}

func (g *Guard) tableStats(ctx context.Context) (map[string]tableStat, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT TABLE_NAME, COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0), COALESCE(AVG_ROW_LENGTH,0)
		 FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`, g.dbName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]tableStat)
	for rows.Next() {
		var name string
		var s tableStat
		if err := rows.Scan(&name, &s.rows, &s.bytes, &s.avgRow); err != nil {
			return nil, err
		}
		out[name] = s
	}
	return out, rows.Err()
}

// CheckColumns 执行列级最大字节扫描（全表扫描 MAX(LENGTH(col))，仅低频或人工排障调用）。
func (g *Guard) CheckColumns(ctx context.Context) Result {
	registerMetrics()
	res := Result{}
	if g == nil || g.db == nil {
		return res
	}
	for _, c := range g.columns {
		var maxLen, avgLen sql.NullInt64
		err := g.db.QueryRowContext(ctx,
			"SELECT MAX(LENGTH(`"+c.Column+"`)), AVG(LENGTH(`"+c.Column+"`)) FROM `"+c.Table+"`").
			Scan(&maxLen, &avgLen)
		if err != nil {
			logx.WithContext(ctx).Errorf("[dbguard] Column size scan failed db=%s table=%s column=%s: %v",
				g.dbName, c.Table, c.Column, err)
			continue
		}
		res.Checked++
		columnMaxBytes.WithLabelValues(g.dbName, c.Table, c.Column).Set(float64(maxLen.Int64))
		if c.MaxBytes <= 0 || maxLen.Int64 <= c.MaxBytes {
			continue
		}
		budgetViolations.WithLabelValues(g.dbName, c.Table, "column_bytes").Inc()
		res.Violations = append(res.Violations, Violation{
			Table: c.Table, Column: c.Column, Kind: "column_bytes",
			Actual: maxLen.Int64, Budget: c.MaxBytes, Note: c.Note,
		})
		logx.WithContext(ctx).Errorw("db_column_size_budget_exceeded",
			logx.Field("db", g.dbName),
			logx.Field("table", c.Table),
			logx.Field("column", c.Column),
			logx.Field("max_bytes", maxLen.Int64),
			logx.Field("avg_bytes", avgLen.Int64),
			logx.Field("budget", c.MaxBytes),
			logx.Field("note", c.Note),
		)
	}
	return res
}

// LargeRow 表示单列字节数最大的定位记录。
type LargeRow struct {
	PK    string
	Bytes int64
}

// TopLargeRows 定位指定列字节数最大的前 N 行（表名与列名必须由调用方代码写死）。
func TopLargeRows(ctx context.Context, db *sql.DB, table, pkCol, col string, limit int) ([]LargeRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx,
		"SELECT `"+pkCol+"`, LENGTH(`"+col+"`) AS n FROM `"+table+"` ORDER BY n DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("dbguard: query top large rows %s.%s: %w", table, col, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LargeRow
	for rows.Next() {
		var r LargeRow
		var n sql.NullInt64
		if err := rows.Scan(&r.PK, &n); err != nil {
			return nil, err
		}
		r.Bytes = n.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}
