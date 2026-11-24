// sweep.go —— 保留期清理统一执行模式与单 WHERE 防漂移封装（移植自 xuanming-server/pkg/dbguard/sweep.go，
// 并结合本仓 friend/sweep_repo.go 的无符号截止点防溢出护栏）。
package dbguard

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// MaxRetentionDays 是保留期天数的防溢出护栏（100 年），防止乘法回绕成负数导致无符号列误删全表。
const (
	MaxRetentionDays = 36500
	millisPerDay     = int64(24 * 60 * 60 * 1000)
)

// Mode 是保留期清理执行模式；零值即 ModeReportOnly（默认只统计不删除）。
type Mode int32

const (
	ModeReportOnly Mode = 0
	ModeDelete     Mode = 1
)

// String 返回低基数模式名。
func (m Mode) String() string {
	if m == ModeDelete {
		return "delete"
	}
	return "report_only"
}

// ParseMode 解析配置里的清理模式；拼写错误直接返回错误（绝不猜成 delete）。
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "report_only", "report", "report-only":
		return ModeReportOnly, nil
	case "delete":
		return ModeDelete, nil
	default:
		return ModeReportOnly, fmt.Errorf("dbguard: invalid retention mode %q (expected report_only or delete)", s)
	}
}

// SweepCutoffMs 计算毫秒级清理截止点；时钟异常导致非正截止点时返回 ok=false 且 err=nil（跳过本轮）。
func SweepCutoffMs(retentionDays, batchLimit int, nowMs int64) (cutoff uint64, ok bool, err error) {
	if batchLimit <= 0 {
		return 0, false, fmt.Errorf("dbguard: sweep batchLimit must be positive, got %d", batchLimit)
	}
	if retentionDays <= 0 || retentionDays > MaxRetentionDays {
		return 0, false, fmt.Errorf("dbguard: sweep retentionDays must be within [1, %d], got %d", MaxRetentionDays, retentionDays)
	}
	cutoffMs := nowMs - int64(retentionDays)*millisPerDay
	if cutoffMs <= 0 {
		return 0, false, nil
	}
	return uint64(cutoffMs), true, nil
}

// Outcome 表示单次清理调用的结果。
type Outcome struct {
	Mode      Mode
	Matched   int64
	Deleted   int64
	Truncated bool
}

// Cleaned 报告本次是否实际删除了数据（report_only 恒为 false）。
func (o Outcome) Cleaned() bool { return o.Deleted > 0 }

var (
	retentionPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mmorpg_db_retention_pending_rows",
		Help: "Number of rows eligible for retention cleanup in report_only mode.",
	}, []string{"db", "table"})

	retentionDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mmorpg_db_retention_deleted_rows_total",
		Help: "Total number of rows deleted by retention cleanup in delete mode.",
	}, []string{"db", "table"})
)

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SweepTable 按 mode 执行单表保留期清理，强制 COUNT 与 DELETE 共享同一 where 与 args 防止条件漂移。
// table 与 where 必须为调用方代码字面量，禁止外部拼接。
func SweepTable(ctx context.Context, db execer, mode Mode, dbLabel, table, where string, limit int, args ...any) (Outcome, error) {
	registerMetrics()
	out := Outcome{Mode: mode}

	if mode != ModeDelete {
		var n int64
		q := "SELECT COUNT(*) FROM `" + table + "` WHERE " + where
		if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return out, fmt.Errorf("dbguard: count pending rows for %s: %w", table, err)
		}
		out.Matched = n
		retentionPending.WithLabelValues(dbLabel, table).Set(float64(n))
		if n > 0 {
			logx.WithContext(ctx).Sloww("db_retention_pending_not_deleted",
				logx.Field("db", dbLabel),
				logx.Field("table", table),
				logx.Field("pending_rows", n),
				logx.Field("mode", mode.String()),
			)
		}
		return out, nil
	}

	if limit <= 0 {
		return out, fmt.Errorf("dbguard: delete limit must be positive for %s, got %d", table, limit)
	}
	q := "DELETE FROM `" + table + "` WHERE " + where + " LIMIT ?"
	res, err := db.ExecContext(ctx, q, append(append([]any(nil), args...), limit)...)
	if err != nil {
		return out, fmt.Errorf("dbguard: delete expired rows for %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	out.Matched, out.Deleted = n, n
	out.Truncated = n >= int64(limit)
	if n > 0 {
		retentionDeleted.WithLabelValues(dbLabel, table).Add(float64(n))
		logx.WithContext(ctx).Infow("db_retention_deleted",
			logx.Field("db", dbLabel),
			logx.Field("table", table),
			logx.Field("deleted", n),
			logx.Field("batch_limit", limit),
			logx.Field("truncated", out.Truncated),
		)
	}
	return out, nil
}
