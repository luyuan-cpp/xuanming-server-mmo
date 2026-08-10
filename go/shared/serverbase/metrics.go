package serverbase

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

const metricSubsystem = "rpc"

// rpcDurationBuckets 覆盖到 5s:本仓 RPC 里最长的一档是登录链路的
// ADMIT_BARRIER 级别等待,再长就不必细分了。
var rpcDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5,
}

var (
	// inbandFaultTotal 只统计判成"服务端内部故障"的 in-band 失败。
	// label 里带 code 是安全的:能进这里的码被故障码集合限死(几十个),
	// 与 method 的笛卡尔积仍然有界。
	inbandFaultTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: metricSubsystem,
		Name:      "inband_fault_total",
		Help:      "响应体里的业务码判定为服务端内部故障的次数。source: error_code | tip_info。",
	}, []string{"method", "source", "code"})

	// inbandRejectTotal 统计正常业务拒绝。刻意**不带 code**:
	// 业务拒绝码远多于故障码,而且它们本来就不需要告警,粗粒度足够。
	inbandRejectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: metricSubsystem,
		Name:      "inband_reject_total",
		Help:      "响应体里的业务码判定为正常业务拒绝的次数(不告警,仅用于看比例)。",
	}, []string{"method", "source"})

	// inbandUnknownCodeTotal 统计"码不在已知码表范围内"。恒 0 是健康态;
	// 一旦非 0 说明 tip 码表长了新段而 serverbase 没跟上。
	inbandUnknownCodeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: metricSubsystem,
		Name:      "inband_unknown_code_total",
		Help:      "响应体里的业务码超出已知码表范围的次数(码表漂移信号,正常应恒 0)。",
	}, []string{"method", "source"})

	// rpcDurationSeconds 是统一的 RPC 耗时直方图。
	// status 把 in-band 定性也算进去,于是"成功率"终于能按真实语义算,
	// 而不是永远 100%。
	rpcDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: metricSubsystem,
		Name:      "duration_seconds",
		Help:      "gRPC 服务端处理耗时。status: ok | biz_reject | fault | unknown_code | transport_error。",
		Buckets:   rpcDurationBuckets,
	}, []string{"method", "status"})

	registerOnce sync.Once
)

// statusTransportError 是 handler 真的返回了非 nil error 时的 status label。
// 与四种 in-band 定性并列,便于一眼区分"框架层错"和"业务体里错"。
const statusTransportError = "transport_error"

// register 惰性注册。shared 是被所有服务 import 的库,
// 在 init 里 MustRegister 会让"只是引用了本包"的进程也被迫背上指标,
// 而且撞名就直接 panic 起不来 —— 所以放到第一次观测时再注册,
// 且用 Register + AlreadyRegisteredError 容错,绝不 panic。
func register() {
	registerOnce.Do(func() {
		collectors := []prometheus.Collector{
			inbandFaultTotal,
			inbandRejectTotal,
			inbandUnknownCodeTotal,
			rpcDurationSeconds,
		}
		for _, c := range collectors {
			if err := prometheus.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[serverbase] 指标注册失败: %v", err)
				}
			}
		}
	})
}

// observe 记录一次 RPC 的定性结果与耗时。
func observe(method string, bc BizCode, verdict Verdict, elapsed time.Duration) {
	register()

	switch verdict {
	case VerdictFault:
		inbandFaultTotal.WithLabelValues(
			method, bc.Source.String(), strconv.FormatUint(uint64(bc.Code), 10)).Inc()
	case VerdictBizReject:
		inbandRejectTotal.WithLabelValues(method, bc.Source.String()).Inc()
	case VerdictUnknown:
		inbandUnknownCodeTotal.WithLabelValues(method, bc.Source.String()).Inc()
	}

	rpcDurationSeconds.WithLabelValues(method, verdict.String()).Observe(elapsed.Seconds())
}

// observeTransportError 记录一次 handler 返回非 nil error 的调用。
func observeTransportError(method string, elapsed time.Duration) {
	register()
	rpcDurationSeconds.WithLabelValues(method, statusTransportError).Observe(elapsed.Seconds())
}
