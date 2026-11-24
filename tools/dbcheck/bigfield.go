// bigfield.go —— 单行大字段深度体检（移植自 xuanming-server/tools/migrate/cmd/dbcheck/bigfield.go）。
//
// 同时度量 AVG(LENGTH(col))（全体变胖信号）、MAX(LENGTH(col))（个体畸形信号）与超预算行数，
// 避免只看行数漏掉单行 BLOB 膨胀。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

type fieldBudget struct {
	DB       string
	Table    string
	Column   string
	PK       string
	MaxBytes int64
	Why      string
}

// defaultBigFields 覆盖本仓已核对的 BLOB/MEDIUMBLOB 业务载荷列。
var defaultBigFields = []fieldBudget{
	{
		DB: "mmorpg_player_inbox", Table: "player_inbox_item", Column: "payload", PK: "seq",
		MaxBytes: 64 * 1024,
		Why:      "player_inbox_item.payload should stay bounded by inbox registry max payload limits",
	},
	{
		DB: "mmorpg_chat", Table: "chat_send_authority", Column: "request_payload", PK: "request_id",
		MaxBytes: 16 * 1024,
		Why:      "chat_send_authority.request_payload is MEDIUMBLOB but chat messages should remain under 16KiB",
	},
	{
		DB: "mmorpg_chat", Table: "chat_send_authority", Column: "message_payload", PK: "request_id",
		MaxBytes: 16 * 1024,
		Why:      "chat_send_authority.message_payload is MEDIUMBLOB but serialized chat frames should remain under 16KiB",
	},
}

type fieldStat struct {
	budget   fieldBudget
	rows     int64
	maxBytes int64
	avgBytes int64
	overRows int64
	err      error
}

func checkBigFields(ctx context.Context, db *sql.DB, fields []fieldBudget, present map[string]map[string]bool) []fieldStat {
	var out []fieldStat
	for _, b := range fields {
		if !present[b.DB][b.Table] {
			continue
		}
		s := fieldStat{budget: b}
		q := fmt.Sprintf(
			"SELECT COUNT(*), COALESCE(MAX(LENGTH(`%s`)),0), COALESCE(AVG(LENGTH(`%s`)),0), "+
				"COALESCE(SUM(CASE WHEN LENGTH(`%s`) > ? THEN 1 ELSE 0 END),0) FROM `%s`.`%s`",
			b.Column, b.Column, b.Column, b.DB, b.Table)
		var avg sql.NullFloat64
		if err := db.QueryRowContext(ctx, q, b.MaxBytes).Scan(&s.rows, &s.maxBytes, &avg, &s.overRows); err != nil {
			s.err = err
		}
		s.avgBytes = int64(avg.Float64)
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri := float64(out[i].maxBytes) / float64(max64(out[i].budget.MaxBytes, 1))
		rj := float64(out[j].maxBytes) / float64(max64(out[j].budget.MaxBytes, 1))
		return ri > rj
	})
	return out
}

type largeRow struct {
	PK    string
	Bytes int64
}

func topLargeRows(ctx context.Context, db *sql.DB, b fieldBudget, limit int) ([]largeRow, error) {
	if b.PK == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		"SELECT CAST(`%s` AS CHAR), LENGTH(`%s`) AS n FROM `%s`.`%s` ORDER BY n DESC LIMIT ?",
		b.PK, b.Column, b.DB, b.Table), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []largeRow
	for rows.Next() {
		var r largeRow
		var n sql.NullInt64
		if err := rows.Scan(&r.PK, &n); err != nil {
			return nil, err
		}
		r.Bytes = n.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
