// payload.go —— 写入侧三档 payload 字节守护（移植自 xuanming-server/pkg/dbguard/payload.go）。
//
// 三档语义：
//   - len(payload) > Max             → fail-closed 拒写 + ERROR 日志 + reject counter；
//   - len(payload) >= Max*WarnRatio  → 放行 + WARN/Slow 日志（在爆表前给出排查窗口）；
//   - 否则                            → 静默放行，仅记录分布直方图（通过 p99 与 max 区分个例与系统性膨胀）。
package dbguard

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// WarnRatio 是逼近上限告警线（占 Max 的 80%）。
const WarnRatio = 0.8

var (
	payloadBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mmorpg_db_payload_bytes",
		Help:    "Distribution of serialized payload byte sizes written to DB columns.",
		Buckets: prometheus.ExponentialBuckets(64, 2, 11),
	}, []string{"db", "table", "column"})

	payloadRejects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mmorpg_db_payload_rejected_total",
		Help: "Count of writes rejected fail-closed due to exceeding payload byte limit.",
	}, []string{"db", "table", "column"})
)

// PayloadLimit 描述单个大字段列的写入侧字节上限。
// Max 必须按设计期望值设定且 <= 列类型容量；AllowEqual 为 false 时（默认）>= Max 拒写，
// 为 true 时允许恰好等于 Max（对齐 inboxcontract 等 `len(payload) > max` 契约）。
type PayloadLimit struct {
	DB         string
	Table      string
	Column     string
	Max        int
	AllowEqual bool
	Hint       string
}

// CheckPayload 在落库前对序列化 payload 执行三档字节数检查。
func CheckPayload(ctx context.Context, limit PayloadLimit, payload []byte) error {
	registerMetrics()
	size := len(payload)
	payloadBytes.WithLabelValues(limit.DB, limit.Table, limit.Column).Observe(float64(size))
	if limit.Max <= 0 {
		return nil
	}
	exceeded := size >= limit.Max
	if limit.AllowEqual {
		exceeded = size > limit.Max
	}
	if exceeded {
		payloadRejects.WithLabelValues(limit.DB, limit.Table, limit.Column).Inc()
		logx.WithContext(ctx).Errorw("db_payload_too_large_rejected",
			logx.Field("db", limit.DB),
			logx.Field("table", limit.Table),
			logx.Field("column", limit.Column),
			logx.Field("size", size),
			logx.Field("max", limit.Max),
			logx.Field("hint", limit.Hint),
		)
		return fmt.Errorf("dbguard: payload too large for %s.%s (%d bytes, limit %d)", limit.Table, limit.Column, size, limit.Max)
	}
	if float64(size) >= float64(limit.Max)*WarnRatio {
		logx.WithContext(ctx).Sloww("db_payload_approaching_limit",
			logx.Field("db", limit.DB),
			logx.Field("table", limit.Table),
			logx.Field("column", limit.Column),
			logx.Field("size", size),
			logx.Field("max", limit.Max),
			logx.Field("ratio", float64(size)/float64(limit.Max)),
			logx.Field("hint", limit.Hint),
		)
	}
	return nil
}
