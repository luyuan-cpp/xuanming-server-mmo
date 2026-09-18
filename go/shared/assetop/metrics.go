package assetop

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	assetpb "proto/common/asset"
)

// 指标(规格 §4.19 metrics.go、§4.41 的追加项)。
//
// 铁律:label 全部低基数。**player_id / op_id / seq 一律不进 label**(AGENTS §11.3),
// 它们只进日志。要按玩家查一笔操作,走日志里的 op_id 或 scene 流水的 correlation_id。
//
// service 做成 ConstLabels:一个进程里只有一个 assetop 使用者,省得每个调用点都传一次
// 却又可能传错。

// Metrics 是本包全部指标的持有者。
//
// 允许为 nil:所有方法都做了 nil 判断,这样「不接指标」的调用方(单测、工具)
// 不必造一个假的注册表。
type Metrics struct {
	rpcTotal           *prometheus.CounterVec
	rpcSeconds         *prometheus.HistogramVec
	requeryTotal       *prometheus.CounterVec
	finalizeTotal      *prometheus.CounterVec
	rescheduleTotal    *prometheus.CounterVec
	unknownTotal       *prometheus.CounterVec
	outcomeFlipTotal   *prometheus.CounterVec
	partialTotal       *prometheus.CounterVec
	claimTotal         *prometheus.CounterVec
	ledgerReadTotal    *prometheus.CounterVec
	manualResolveTotal *prometheus.CounterVec
	storeErrorsTotal   *prometheus.CounterVec
	pendingOldestAge   *prometheus.GaugeVec
}

// NewMetrics 在 reg 上注册本包的全部指标。
// reg 为 nil 时只构造不注册(promauto 的既定行为),单测可以放心用。
func NewMetrics(reg prometheus.Registerer, service string) *Metrics {
	f := promauto.With(reg)
	labels := prometheus.Labels{"service": service}
	return &Metrics{
		rpcTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_rpc_total",
			Help:        "资产 RPC 次数,按结局分。error=传输层失败,no_location=没发出去",
			ConstLabels: labels,
		}, []string{"stream", "rpc", "outcome"}),
		rpcSeconds: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "assetop_rpc_seconds",
			Help:        "单次资产 RPC 耗时(含 scene loop 线程排队)",
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
			ConstLabels: labels,
		}, []string{"rpc"}),
		requeryTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_requery_total",
			Help:        "durable 重查结果:durable=等到了落盘,timeout=预算用完仍未落盘,error=重查出错",
			ConstLabels: labels,
		}, []string{"rpc", "result"}),
		finalizeTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_finalize_total",
			Help:        "终结的 outbox 行数,按最终状态分",
			ConstLabels: labels,
		}, []string{"stream", "status"}),
		rescheduleTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_reschedule_total",
			Help:        "重排次数:await_durable / retry / alert",
			ConstLabels: labels,
		}, []string{"stream", "reason"}),
		unknownTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_unknown_total",
			Help:        "scene 回 UNKNOWN 的次数(配置不一致 / 纪元过期 / 跳号 / 验签失败),必须告警",
			ConstLabels: labels,
		}, []string{"stream"}),
		outcomeFlipTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_outcome_flip_total",
			Help:        "同一 seq 两次查询给出不同终结结局的次数,违反不变量 I2,必须告警",
			ConstLabels: labels,
		}, []string{"stream"}),
		partialTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_partial_total",
			Help:        "部分发放次数;对侧账不做,须人工补偿",
			ConstLabels: labels,
		}, []string{"stream"}),
		claimTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_claim_total",
			Help:        "领取单行的结果:claimed / lost(被别的副本领走或已终结)/ poison(payload 解不开)",
			ConstLabels: labels,
		}, []string{"result"}),
		ledgerReadTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_ledger_read_total",
			Help:        "离线读已落盘账本的结果:finalized / unseen / absent / error",
			ConstLabels: labels,
		}, []string{"result"}),
		manualResolveTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_manual_resolve_total",
			Help:        "人工终结次数,按最终状态分",
			ConstLabels: labels,
		}, []string{"status"}),
		storeErrorsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name:        "assetop_store_errors_total",
			Help:        "业务存储出错次数:list / claim / finalize / reschedule / decode / ledger_read",
			ConstLabels: labels,
		}, []string{"op"}),
		pendingOldestAge: f.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "assetop_pending_oldest_age_seconds",
			Help:        "最老未决行的年龄;超过业务截止时间很久就是卡死行",
			ConstLabels: labels,
		}, []string{"stream"}),
	}
}

// streamLabels 是流号到 label 的平铺表,下标即枚举值。
var streamLabels = [...]string{
	"unspecified",
	"guild_debit",
	"guild_credit",
	"trade_debit",
	"trade_credit",
	"system_credit",
}

// 编译期断言:表长必须正好覆盖到最后一个流。新增流忘了加 label 时这行报错,
// 而不是等到线上看见一堆 "other"。
var _ = [1]struct{}{}[len(streamLabels)-1-int(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT)]

// StreamLabel 把流号转成低基数 label;越界值统一归到 "other",绝不把原始数值放进 label。
func StreamLabel(s assetpb.AssetOpStream) string {
	if s < 0 || int(s) >= len(streamLabels) {
		return "other"
	}
	return streamLabels[s]
}

// outcomeLabels 下标即 AssetOpOutcome 的枚举值。
var outcomeLabels = [...]string{"unknown", "applied", "rejected", "retry", "not_here"}

var _ = [1]struct{}{}[len(outcomeLabels)-1-int(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE)]

// 两个不来自 scene 的结局 label:传输层失败,以及压根没发出去。
const (
	outcomeLabelError      = "error"
	outcomeLabelNoLocation = "no_location"
)

func outcomeLabel(o assetpb.AssetOpOutcome) string {
	if o < 0 || int(o) >= len(outcomeLabels) {
		return "other"
	}
	return outcomeLabels[o]
}

func (m *Metrics) observeRPC(stream assetpb.AssetOpStream, rpc RPC, outcome string, seconds float64) {
	if m == nil {
		return
	}
	m.rpcTotal.WithLabelValues(StreamLabel(stream), rpc.String(), outcome).Inc()
	m.rpcSeconds.WithLabelValues(rpc.String()).Observe(seconds)
}

// incRPCNoLocation 记一次「没发出去」。它不进耗时直方图:没有网络往返可言。
func (m *Metrics) incRPCNoLocation(stream assetpb.AssetOpStream, rpc RPC) {
	if m == nil {
		return
	}
	m.rpcTotal.WithLabelValues(StreamLabel(stream), rpc.String(), outcomeLabelNoLocation).Inc()
}

func (m *Metrics) incRequery(rpc RPC, result string) {
	if m == nil {
		return
	}
	m.requeryTotal.WithLabelValues(rpc.String(), result).Inc()
}

func (m *Metrics) incFinalize(stream assetpb.AssetOpStream, status Status) {
	if m == nil {
		return
	}
	m.finalizeTotal.WithLabelValues(StreamLabel(stream), status.String()).Inc()
}

func (m *Metrics) incReschedule(stream assetpb.AssetOpStream, reason string) {
	if m == nil {
		return
	}
	m.rescheduleTotal.WithLabelValues(StreamLabel(stream), reason).Inc()
}

func (m *Metrics) incUnknown(stream assetpb.AssetOpStream) {
	if m == nil {
		return
	}
	m.unknownTotal.WithLabelValues(StreamLabel(stream)).Inc()
}

func (m *Metrics) incOutcomeFlip(stream assetpb.AssetOpStream) {
	if m == nil {
		return
	}
	m.outcomeFlipTotal.WithLabelValues(StreamLabel(stream)).Inc()
}

func (m *Metrics) incPartial(stream assetpb.AssetOpStream) {
	if m == nil {
		return
	}
	m.partialTotal.WithLabelValues(StreamLabel(stream)).Inc()
}

func (m *Metrics) incClaim(result string) {
	if m == nil {
		return
	}
	m.claimTotal.WithLabelValues(result).Inc()
}

func (m *Metrics) incLedgerRead(result string) {
	if m == nil {
		return
	}
	m.ledgerReadTotal.WithLabelValues(result).Inc()
}

func (m *Metrics) incManualResolve(status Status) {
	if m == nil {
		return
	}
	m.manualResolveTotal.WithLabelValues(status.String()).Inc()
}

func (m *Metrics) incStoreError(op string) {
	if m == nil {
		return
	}
	m.storeErrorsTotal.WithLabelValues(op).Inc()
}

func (m *Metrics) setPendingOldestAge(stream assetpb.AssetOpStream, seconds float64) {
	if m == nil {
		return
	}
	m.pendingOldestAge.WithLabelValues(StreamLabel(stream)).Set(seconds)
}
