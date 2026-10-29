package logic

import "sync"

// 领导权闸门:scene_manager 多副本部署时,**变更类**后台动作(补建世界频道 /
// rebalance / 死节点孤儿清理 / 空闲副本销毁 / 世界频道自动扩缩容 / Agones
// 对账 / 负载分刷新)只允许领导者执行;**数据面**(RPC handler、etcd watch 内存镜像)
// 每个副本照常跑 —— 它们的权威状态在 Redis,写入要么走 Lua CAS 要么是幂等同值写,
// 天然多实例安全。
//
// 负载分刷新不是幂等同值写:跟随者的 watch 镜像可能落后于领导者的清理,会把领导者刚摘掉的
// 死节点重新 ZADD 回负载集(理由见 load_reporter.go updateNodeLoad)。2026-10-16 起跟随者连读
// 也不做,每节点指标只由领导者导出,跟随者只发布本 zone 的 nodes_by_role(load_refresh.go)。
//
// 领导者由 shared/leader 的 Redis 锁选出,接线在 scene_manager_service.go。
// 未接线时(单测、旧启动路径)一律视为领导者,与历史单实例行为完全一致。
var (
	leaderCheckMu sync.RWMutex
	leaderCheckFn func() bool
)

// SetLeaderCheck 安装领导权判定函数(通常传 leader.Elector 的 IsLeader)。
// 必须在后台循环启动之前调用。
func SetLeaderCheck(f func() bool) {
	leaderCheckMu.Lock()
	leaderCheckFn = f
	leaderCheckMu.Unlock()
}

// isLeader 报告本副本当前是否允许执行变更类后台动作。
func isLeader() bool {
	leaderCheckMu.RLock()
	f := leaderCheckFn
	leaderCheckMu.RUnlock()
	if f == nil {
		return true
	}
	return f()
}

// SetLeaderCheckForTest 安装假领导权判定并返回恢复函数,须配合 t.Cleanup。
func SetLeaderCheckForTest(f func() bool) (restore func()) {
	leaderCheckMu.Lock()
	prev := leaderCheckFn
	leaderCheckFn = f
	leaderCheckMu.Unlock()
	return func() {
		leaderCheckMu.Lock()
		leaderCheckFn = prev
		leaderCheckMu.Unlock()
	}
}

// loadReporterResync 用于踢 load reporter 立即重跑 fullSync。两个来源:领导权变化(新任领导者把跟随者期间跳过的变更
// 动作 —— 补频道 / rebalance / 清理 —— 一次补齐);推迟摘除放弃时(retryDeferredNodeDetaches 的 abandoned 分支,
// 让 fullSync 的陈旧清扫把还留在负载集里的死节点重新推迟)。
// 容量 1 + 非阻塞发送:合并密集信号,发送方永不阻塞。
var loadReporterResync = make(chan struct{}, 1)

// RequestLoadReporterResync 请求 load reporter 尽快做一次 fullSync。
func RequestLoadReporterResync() {
	select {
	case loadReporterResync <- struct{}{}:
	default:
	}
}
