package noderegistry

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// 注册层的低基数指标(docs/design/infra-reconnect-overview.md §1.8「连接状态、重连次数、按原因分的
// 失败次数」,缺口 R17)。它们回答「本进程此刻在不在服务发现里、多久掉一次、为什么回不来」:
//   - noderegistry_registered 为 0:失租后还没重注册成功(或已 Close)。路由服 / C++ 发现不到本实例,
//     进程却活着、也不报错 —— R4 说的「从发现里消失而且没有告警」就靠它告警;
//   - lease_lost_total 涨得快:租约在抖(etcd 慢、网络分区、LeaseTTL 配得太短);
//   - reregister_failure_total{reason}:卡在哪一步;
//   - node_id_reallocated_total 涨:原号被别的实例接管过,本进程已换号,发现键路径随之变了。
//
// label 只有 prefix(Spec.Prefix = 节点类型枚举名 + ".rpc",部署期常量)与 reason(下面的固定集合)。
// 不放 node_id / uuid / lease:换号后 node_id 会变、uuid 每个进程一个,序列数会随重启与换号增长(AGENTS.md §9)。
//
// 惰性注册,理由同 shared/safego、shared/kafkautil:本包被多个服务 import,init 期 MustRegister 撞名会让进程起不来。

// 重注册失败原因,即 reason label 的全部取值。
const (
	reRegisterFailGrant     = "grant"      // 申请新租约失败;退避后重试
	reRegisterFailReclaim   = "reclaim"    // 重夺原 node_id 的 Txn 失败(传输错误或超时);退避后重试
	reRegisterFailAllocate  = "allocate"   // 原号已归别人,分配新 node_id 失败;退避后重试
	reRegisterFailTakenOver = "taken_over" // 原号已归别人且策略是 ExitProcess;进程随即退出
)

var reRegisterFailReasons = []string{reRegisterFailGrant, reRegisterFailReclaim, reRegisterFailAllocate, reRegisterFailTakenOver}

var (
	leaseLostTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "noderegistry_lease_lost_total",
		Help: "Times a node registration lost its etcd lease (keepalive stream ended or could not start) and started re-registering, by registry prefix.",
	}, []string{"prefix"})
	reRegisterSuccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "noderegistry_reregister_success_total",
		Help: "Re-registrations committed on a new lease after a lease loss (original node_id reclaimed or a new one allocated), by registry prefix.",
	}, []string{"prefix"})
	reRegisterFailureTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "noderegistry_reregister_failure_total",
		Help: "Failed re-registration attempts by registry prefix and reason: grant|reclaim|allocate are retried with backoff; taken_over means the original node_id belongs to another instance under policy ExitProcess and the process exits.",
	}, []string{"prefix", "reason"})
	nodeIDReallocatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "noderegistry_node_id_reallocated_total",
		Help: "Re-registrations that switched to a new node_id because another instance had taken the original one, by registry prefix.",
	}, []string{"prefix"})
	registeredGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "noderegistry_registered",
		Help: "Registrations in this process currently holding their discovery keys on a live lease, by registry prefix: normally 1 while serving, 0 between a lease loss and the next successful re-registration and after Close.",
	}, []string{"prefix"})

	metricsOnce sync.Once
)

func registerMetrics() {
	metricsOnce.Do(func() {
		for _, c := range []prometheus.Collector{leaseLostTotal, reRegisterSuccessTotal, reRegisterFailureTotal, nodeIDReallocatedTotal, registeredGauge} {
			if err := prometheus.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					logx.Errorf("[noderegistry] failed to register metrics: %v", err)
				}
			}
		}
	})
}

// primeMetrics 在注册成功时把本前缀的全部序列建出来(计数器为 0,gauge 由随后的提交置 1):
// 否则第一次失租 / 失败之前序列不存在,`increase(...) > 0` 恰好漏掉第一次。
func primeMetrics(prefix string) {
	registerMetrics()
	leaseLostTotal.WithLabelValues(prefix).Add(0)
	reRegisterSuccessTotal.WithLabelValues(prefix).Add(0)
	for _, reason := range reRegisterFailReasons {
		reRegisterFailureTotal.WithLabelValues(prefix, reason).Add(0)
	}
	nodeIDReallocatedTotal.WithLabelValues(prefix).Add(0)
	registeredGauge.WithLabelValues(prefix).Add(0)
}

func observeLeaseLost(prefix string) {
	registerMetrics()
	leaseLostTotal.WithLabelValues(prefix).Inc()
}

func observeReRegisterSuccess(prefix string) {
	registerMetrics()
	reRegisterSuccessTotal.WithLabelValues(prefix).Inc()
}

func observeReRegisterFailure(prefix, reason string) {
	registerMetrics()
	reRegisterFailureTotal.WithLabelValues(prefix, reason).Inc()
}

func observeNodeIDReallocated(prefix string) {
	registerMetrics()
	nodeIDReallocatedTotal.WithLabelValues(prefix).Inc()
}

// addRegistered 调整 noderegistry_registered。只由 Registration.setHeldLocked 调用:每个句柄只在
// 「未持有 → 持有」时 +1、「持有 → 未持有」时 -1,同一进程同一前缀有几份注册就加几次。
func addRegistered(prefix string, delta float64) {
	registerMetrics()
	registeredGauge.WithLabelValues(prefix).Add(delta)
}
