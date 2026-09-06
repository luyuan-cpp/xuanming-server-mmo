// Package metrics 暴露路由服的 Prometheus 指标(设计文档 §6):
//   - client_rpc_router_forward_total{message_id, outcome}   每次 Forward 的终态;
//   - client_rpc_router_forward_seconds{message_id}          Forward 端到端耗时;
//   - client_rpc_router_targets{node_type}                   各目标类型当前发现到的实例数。
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
const (
	OutcomeOK                = "ok"                  // 目标正常响应,原始字节已回包
	OutcomeMissingSession    = "missing_session"     // 缺 x-session-detail-bin,gRPC UNAUTHENTICATED
	OutcomeInvalidRequest    = "invalid_request"     // ForwardRequest.request 为空
	OutcomeUnknownMessage    = "unknown_message"     // 消息号不在路由表
	OutcomeNotClientProtocol = "not_client_protocol" // 所属 service 不是客户端协议
	OutcomeBattleRejected    = "battle_rejected"     // 目标是 Battle,战斗只走直连(D33)
	OutcomeNoTarget          = "no_target"           // 目标类型没有可用实例(zone-scoped 时指同 zone 内)
	OutcomeDialError         = "dial_error"          // 建目标连接失败
	OutcomeUpstreamTimeout   = "upstream_timeout"    // 目标在 ForwardTimeoutMs 内未响应
	OutcomeUpstreamError     = "upstream_error"      // 目标返回其它 gRPC 错误
)

// UnknownMessageIdLabel 是不在路由表里的消息号统一使用的 label 值。
const UnknownMessageIdLabel = "unknown"

var (
	forwardTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "forward_total",
		Help:      "Forward calls by client message id and outcome (ok|missing_session|invalid_request|unknown_message|not_client_protocol|battle_rejected|no_target|dial_error|upstream_timeout|upstream_error).",
	}, []string{"message_id", "outcome"})

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
)

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(forwardTotal, forwardSeconds, targets)
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

// ObserveForward 记录一次 Forward 的终态与耗时。
func ObserveForward(messageIdLabel string, outcome string, elapsed time.Duration) {
	forwardTotal.WithLabelValues(messageIdLabel, outcome).Inc()
	forwardSeconds.WithLabelValues(messageIdLabel).Observe(elapsed.Seconds())
}

// ForwardTotalValue 读回某 (message_id, outcome) 的计数(单测断言用;计数器本身
// 不导出,避免业务代码绕过 Observe* 直接改指标)。
func ForwardTotalValue(messageIdLabel string, outcome string) float64 {
	var m dto.Metric
	if err := forwardTotal.WithLabelValues(messageIdLabel, outcome).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// SetTargets 更新某目标节点类型当前发现到的实例数(NodeWatcher 的 onCount 回调)。
func SetTargets(nodeType string, count int) {
	targets.WithLabelValues(nodeType).Set(float64(count))
}

// Start 启动 Prometheus /metrics 端点;addr 为空则关闭(与 match 同模式)。
func Start(addr string) {
	if addr == "" {
		logx.Info("[metrics] MetricsListenAddr 为空,不开 Prometheus /metrics")
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
		logx.Infof("[metrics] Prometheus /metrics 监听 %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[metrics] HTTP 服务退出: %v", err)
		}
	}()
}
