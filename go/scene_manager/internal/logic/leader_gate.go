package logic

import (
	"fmt"
	"sync"
)

// leaderLockKeyFmt 是按 zone 的选主锁(docs/design/control-plane-zone-scoping-20261105.md §3.3、§4.2)。
// 原来全服一把 scene_manager:leader:lock,一个领导者编排全部 zone;现在每个 zone 一把,同组几个 zone 的锁互不相干。
// %s 是 sceneControlTag(zone):锁与本 zone 的控制域键同槽(docs/design/redis-cluster-all-domains-20261110.md §4.7),
// 带围栏的写才能在同一段脚本里核对锁键并写数据(leaderFence)。
const leaderLockKeyFmt = "scene_manager:leader:lock:%s"

// LeaderLockKey 返回 zone 的选主锁键 scene_manager:leader:lock:{ctl:<z>}。锁名只由 zone 决定、不可配置。
func LeaderLockKey(zoneID uint32) string {
	return fmt.Sprintf(leaderLockKeyFmt, sceneControlTag(zoneID))
}

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
// 领导者由 shared/leader 的 Redis 锁选出(锁按 zone,见 LeaderLockKey),接线在 scene_manager_service.go。
// isLeader 为真的含义是「本副本是配置 ZoneId 这一个 zone 的领导者」,变更动作只作用于这个 zone(orchestrated_zones.go)。
// 未接线时(单测、旧启动路径)一律视为领导者,与历史单实例行为完全一致。
//
// isLeader 是本进程对自己的判断,会过时:进程卡顿到锁过期,或 Redis 主从切换丢了锁之后,在下一次心跳之前它仍为真,
// 这段时间里可能有两个副本都以为自己是领导者。所以只凭 isLeader 做的写必须满足三者之一:
//   - 本身幂等(同值写);
//   - 带按对象的条件(认领比对属主与 token、改派按期望属主 CAS、扩缩容把判断收在脚本里);
//   - 带围栏(leaderFence):写脚本在同一段里核对选主锁键的值仍是本任期写进去的那个。负载分刷新里会加成员的 ZADD 走这一条。
//
// 新增领导者独占的写时按这三条对号入座,对不上的就带围栏。
var (
	leaderCheckMu sync.RWMutex
	leaderCheckFn func() bool
	// leaderTokenFn 返回本任期写进选主锁的值(leader.Elector 的 Token),不在任时返回空串。见 leaderFence。
	leaderTokenFn func() string
)

// SetLeaderElection 安装领导权判定与围栏值(传 leader.Elector 的 IsLeader 与 Token)。两个一起装:
// 只装判定、不装围栏的话,带围栏的写会退回没有核对的写法。必须在后台循环启动之前调用。
func SetLeaderElection(isLeader func() bool, token func() string) {
	leaderCheckMu.Lock()
	leaderCheckFn = isLeader
	leaderTokenFn = token
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

// leaderFence 返回领导者独占的写要带的围栏:zone 的选主锁键,和本任期写进锁键的值。
// 写脚本先核对锁键的值仍等于它,再写数据(同一段脚本、同一个槽)。由存锁的那台 Redis 在写的那一刻判定谁是属主:
// 锁已过期、已被别的副本拿走,或主从切换之后新主上没有这把锁,带着旧值的写一个键都写不进去。
//
//   - fenced=false:没有接选主(单测、旧启动路径),调用方按没有围栏的写法执行,与历史单实例行为一致;
//   - fenced=true 而 token 为空:本副本此刻已不在任,调用方不得写;
//   - 锁键按 zoneID 取。本副本只是配置 ZoneId 的领导者,拿它的值去写别的 zone,核不过那个 zone 的锁键。
func leaderFence(zoneID uint32) (lockKey, token string, fenced bool) {
	leaderCheckMu.RLock()
	f := leaderTokenFn
	leaderCheckMu.RUnlock()
	if f == nil {
		return "", "", false
	}
	return LeaderLockKey(zoneID), f(), true
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

// SetLeaderFenceForTest 安装假围栏值并返回恢复函数,须配合 t.Cleanup。token 为 nil 表示没有接选主。
func SetLeaderFenceForTest(token func() string) (restore func()) {
	leaderCheckMu.Lock()
	prev := leaderTokenFn
	leaderTokenFn = token
	leaderCheckMu.Unlock()
	return func() {
		leaderCheckMu.Lock()
		leaderTokenFn = prev
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
