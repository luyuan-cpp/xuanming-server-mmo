package callerauth

import "github.com/zeromicro/go-zero/core/metric"

// 指标全部走 go-zero 的 metric 包,由 login.yaml 里既有的 Prometheus 块
// (自带 /metrics)暴露,不另起 promhttp。
//
// # 低基数纪律(仓库 CLAUDE.md §9)
//
// label 只放 result(取值被 Reason 的枚举限死)和 mode(两个取值)。
// **绝不放 caller** —— caller 是调用方自报的字符串,攻击者可以每次换一个,
// 直接把 Prometheus 的时间序列打爆。
const metricNamespace = "login_caller_auth"

var (
	// authTotal 是验签闸门的总账:压测/巡检时
	// result!="ok" 且 mode="enforce" 的量应当恒 0。
	authTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Name:      "total",
		Help: "内部调用方身份声明的验签结果。result=ok 为通过,其余取值见 " +
			"callerauth.Reason;mode=enforce 表示失败会拒绝请求,permissive 表示只告警放行。",
		Labels: []string{"result", "mode"},
	})

	// nonceOverflowTotal > 0 表示 nonce 表被迫提前翻代,
	// 重放窗口临时缩短 —— 该调大 InternalAuth.MaxNonceEntries 了。
	nonceOverflowTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Name:      "nonce_overflow_total",
		Help:      "nonce 表超过 MaxNonceEntries 被迫提前翻代的次数。稳态应恒 0。",
		Labels:    []string{},
	})
)

func recordAuth(result string, enforce bool) {
	mode := "permissive"
	if enforce {
		mode = "enforce"
	}
	authTotal.Inc(result, mode)
}

// RecordNonceOverflow 供 Verifier 的溢出回调使用。
func RecordNonceOverflow() { nonceOverflowTotal.Inc() }
