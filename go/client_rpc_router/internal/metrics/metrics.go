// Package metrics 暴露路由服的 Prometheus 指标(设计文档 §6):
//   - client_rpc_router_forward_total{message_id, outcome}   每次 Forward 的终态;
//   - client_rpc_router_forward_by_track_total{release_track, outcome}
//     同一批终态按 gate 自报的会话轨道再记一份,金丝雀观察拿两条轨道的整体错误率比较
//     (docs/design/release-track-gray-canary.md §2.7)。不带 message_id,基数固定为 3 × 终态数;
//   - client_rpc_router_forward_seconds{message_id}          Forward 端到端耗时;
//   - client_rpc_router_targets{node_type}                   各目标类型当前发现到的实例数;
//   - client_rpc_router_target_conns                         缓存着的目标连接数(每轮空闲回收后采样);
//   - client_rpc_router_idle_target_conns_closed_total       空闲回收关掉的目标连接数。
//     这两个看子集与空闲回收的效果(connection-fanout-subsetting-20261106.md §3.3),不带 label。
//
// message_id 作 label 是有界的:取值只可能是生成路由表里的消息号(gate 白名单
// 放过来的都在表里),不在表里的一律记 "unknown",不让任意数字进 label。
// player_id / session_id 只进日志,不进指标(AGENTS.md §9)。
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "client_rpc_router"

// Forward 终态(outcome label 取值,低基数)。
//
// no_target 对全局类型指「会话轨道可用的实例」:stable 会话只算 stable 实例(发布轨道设计 §2.4)。
const (
	OutcomeOK                  = "ok"                    // 目标正常响应,原始字节已回包
	OutcomeMissingSession      = "missing_session"       // 缺 x-session-detail-bin,gRPC UNAUTHENTICATED
	OutcomeInvalidRequest      = "invalid_request"       // ForwardRequest.request 为空
	OutcomeUnknownMessage      = "unknown_message"       // 消息号不在路由表
	OutcomeNotClientProtocol   = "not_client_protocol"   // 所属 service 不是客户端协议
	OutcomeBattleRejected      = "battle_rejected"       // 目标是 Battle,战斗只走直连(D33)
	OutcomeInvalidReleaseTrack = "invalid_release_track" // gate 自报的 release_track 不是空串 / stable / canary(契约违例)
	OutcomeNoTarget            = "no_target"             // 目标类型没有可用实例(zone-scoped 时指同 zone 内)
	OutcomeDialError           = "dial_error"            // 建目标连接失败
	OutcomeUpstreamTimeout     = "upstream_timeout"      // 目标在 ForwardTimeoutMs 内未响应
	OutcomeUpstreamError       = "upstream_error"        // 目标返回其它 gRPC 错误
)

// UnknownMessageIdLabel 是不在路由表里的消息号统一使用的 label 值。
const UnknownMessageIdLabel = "unknown"

// TrackLabelUnknown:gate 自报的会话轨道认不出时 release_track label 的取值(这类请求按 invalid_release_track 拒绝)。
// 其余请求的 label 是 releasetrack.Track 的 String(),只有 stable、canary 两个值。
const TrackLabelUnknown = "unknown"

var (
	forwardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "forward_total",
		Help:      "Forward calls by client message id and outcome (ok|missing_session|invalid_request|unknown_message|not_client_protocol|battle_rejected|invalid_release_track|no_target|dial_error|upstream_timeout|upstream_error).",
	}, []string{"message_id", "outcome"})

	forwardByTrackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "forward_by_track_total",
		Help:      "Forward calls by the session release track reported by the gate (stable|canary|unknown) and outcome (same values as forward_total).",
	}, []string{"release_track", "outcome"})

	forwardSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "forward_seconds",
		Help:      "End-to-end Forward latency (metadata check -> response bytes ready), by client message id.",
		Buckets:   []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"message_id"})

	targets = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "targets",
		Help:      "Target service instances currently visible in the etcd watch cache, by node type.",
	}, []string{"node_type"})

	targetConns = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "target_conns",
		Help:      "Cached gRPC connections to target service instances, sampled after each idle sweep.",
	})

	idleTargetConnsClosed = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "idle_target_conns_closed_total",
		Help:      "Cached target connections closed because they were unused for the idle timeout.",
	})
)

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(forwardTotal, forwardByTrackTotal, forwardSeconds, targets, targetConns, idleTargetConnsClosed)
	})
}

// MessageIdLabel 把消息号变成 label 值;known=false(不在路由表)一律记 "unknown",
// 防止任意数字把 label 基数撑爆。
func MessageIdLabel(messageId uint32, known bool) string {
	if !known {
		return UnknownMessageIdLabel
	}
	return strconv.FormatUint(uint64(messageId), 10)
}

// ObserveForward 记录一次 Forward 的终态与耗时。trackLabel 是会话轨道(stable / canary,认不出时为 TrackLabelUnknown),
// 两个计数器在这里一起记,每次 Forward 恰好各记一次,按轨道求和与 forward_total 的总数相等。
func ObserveForward(messageIdLabel string, trackLabel string, outcome string, elapsed time.Duration) {
	forwardTotal.WithLabelValues(messageIdLabel, outcome).Inc()
	forwardByTrackTotal.WithLabelValues(trackLabel, outcome).Inc()
	forwardSeconds.WithLabelValues(messageIdLabel).Observe(elapsed.Seconds())
}

// ForwardTotalValue 读回某 (message_id, outcome) 的计数(单测断言用;计数器本身
// 不导出,避免业务代码绕过 Observe* 直接改指标)。
func ForwardTotalValue(messageIdLabel string, outcome string) float64 {
	return counterValue(forwardTotal, messageIdLabel, outcome)
}

// ForwardByTrackValue 读回某 (release_track, outcome) 的计数(单测断言用,同上)。
func ForwardByTrackValue(trackLabel string, outcome string) float64 {
	return counterValue(forwardByTrackTotal, trackLabel, outcome)
}

func counterValue(vec *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// SetTargets 更新某目标节点类型当前发现到的实例数(NodeWatcher 的 onCount 回调)。
func SetTargets(nodeType string, count int) {
	targets.WithLabelValues(nodeType).Set(float64(count))
}

// ObserveIdleConnSweep 记录一轮空闲回收:关了 closed 条,还缓存着 remaining 条。
func ObserveIdleConnSweep(closed, remaining int) {
	idleTargetConnsClosed.Add(float64(closed))
	targetConns.Set(float64(remaining))
}

// Start 启动 Prometheus /metrics 端点;addr 为空则关闭(与 match 同模式)。
func Start(addr string) {
	if addr == "" {
		logx.Info("[metrics] MetricsListenAddr empty; Prometheus /metrics disabled")
		return
	}
	register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("[metrics] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[metrics] HTTP server exited: %v", err)
		}
	}()
}
