package scenenode

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Resolve 结果口径。全是常量字符串,低基数,**不带 player_id**(AGENTS.md §11.3)。
const (
	// ResolveFound:拿到了节点客户端。
	ResolveFound = "found"
	// ResolveNotOnline:位置键不存在。
	ResolveNotOnline = "not_online"
	// ResolveAwaitingPlacement:跨 zone 交接已放行、还没落点 —— 没人持有,重试。
	// 与 not_online 分开:它是传送在途的正常中间态,不该混进"离线率"。
	ResolveAwaitingPlacement = "awaiting_placement"
	// ResolveNodeUnknown:位置记录指向的节点在镜像里查无此节点 —— 节点掉线 /
	// 刚重启还没注册,或位置记录已陈旧。这一档抬高才是"节点掉线"告警的依据。
	ResolveNodeUnknown = "node_unknown"
	// ResolveMirrorUnsynced:镜像还没完成首次全量同步。进程刚起来的几百毫秒里
	// 必然出现,是正常现象;混进 node_unknown 会让每次重启都像一次节点掉线。
	ResolveMirrorUnsynced = "mirror_unsynced"
	// ResolveNodeAmbiguous:同身份多条注册被拒选(脑裂)。与 node_unknown 分开:
	// 前者是"节点没了",这一档是"两个进程抢同一个身份",处置完全不同 ——
	// 必须去查租约与部署链。同时 Watcher.EntryOf 会打 ERROR 日志。
	ResolveNodeAmbiguous = "node_ambiguous"
	// ResolveError:Redis 故障、反序列化失败、拨号失败 —— 需要告警的那一档。
	ResolveError = "error"
)

// Metrics 是本包的指标句柄。实例化而不是包级全局:一个进程可能有多个装配
// (生产 + 测试),而且单测要能拿独立 Registry 断言计数。
//
// 所有方法都可用 nil 接收者调用(Locator.Metrics 允许为 nil),调用方不必判空。
type Metrics struct {
	nodes   *prometheus.GaugeVec
	resolve *prometheus.CounterVec
	service string
}

// NewMetrics 构造并注册指标。service 是调用方服务名("guild" / "trade"),
// 作为固定 label 写进每条指标 —— 同一份 shared 代码在不同服务里跑,指标要能分开。
//
// reg 为 nil 时只构造不注册(promauto.With(nil) 的语义),测试可以直接用。
// 重复注册同名指标会 panic —— 与 promauto 一致,一个进程一份,在装配层建。
func NewMetrics(reg prometheus.Registerer, service string) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		service: service,
		nodes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "scenenode_nodes",
			Help: "etcd 节点镜像里的 scene 节点数量。",
		}, []string{"service", "kind"}),
		resolve: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "scenenode_resolve_total",
			Help: "玩家 → scene 节点定位次数,按结果分:found / not_online / awaiting_placement / node_unknown / mirror_unsynced / node_ambiguous / error。",
		}, []string{"service", "result"}),
	}
}

// SetNodes 上报某个前缀镜像的节点数。签名与 NewWatcher 的 onCount 一致,
// 可以直接当回调传进去;kind 就是 watcher 的 name。
func (m *Metrics) SetNodes(kind string, count int) {
	if m == nil {
		return
	}
	m.nodes.WithLabelValues(m.service, kind).Set(float64(count))
}

// ObserveResolve 记一次定位结果。result 只能取本文件的 Resolve* 常量。
func (m *Metrics) ObserveResolve(result string) {
	if m == nil {
		return
	}
	m.resolve.WithLabelValues(m.service, result).Inc()
}
