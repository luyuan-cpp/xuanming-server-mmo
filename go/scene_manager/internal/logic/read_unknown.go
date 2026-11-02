package logic

import (
	"scene_manager/internal/metrics"

	"github.com/zeromicro/go-zero/core/logx"
)

// 读 Redis 出错时的统一收口(2026-10-22)。
//
// go-zero 的 Redis 读命令有三种结果:有值;确实不存在(Get 把 redis.Nil 吞成 ("", nil),Smembers 回空切片,
// Exists 回 false;Hget / Zscore 不吞,回 redis.Nil);读错(超时、断连、熔断,回其它 err)。以前 `x, _ :=` 的写法
// 把第三种并进第二种:读不到属主就当频道陈旧去改派,读不到 Agones 名额就不还、随后删键,读不到镜像集合
// 就不级联、随后删索引。
//
// 改派 / 销毁 / 清理 / 伸缩这些会改写归属或删键的路径,读错时一律**不按读错的结果行事**:能放弃的本轮放弃、
// 什么都不写,由下一拍或上游重试;已经提交、撤不回的(镜像级联的重读)退回提交前的快照,并保留索引。
// 方向与 reentry_barrier.go 一致:少做一次只是本轮不自愈,做错一次是同一 scene_id 两份实体、映射丢失或名额泄漏。

// readUnknownSite* 是 ownership_read_unknown_total 的 site 取值。**必须是常量**:它们直接变成
// Prometheus label,拼进 scene_id / node_id 会把基数打爆(AGENTS.md §9)。除注明的以外,都是「本轮放弃、没写」。
const (
	// initWorldScenesForZone 读期望频道数 / 频道集合(读不到就不知道缺几个),
	// 或补发 CreateScene 之前读频道集合 / 属主(这一组或这个频道本轮不补发)。
	readUnknownSiteWorldInit = "world_init"
	// GetBestWorldChannel / ReserveBestWorldChannelForEnter 读频道属主:该频道本次不当候选也不当陈旧。
	readUnknownSiteWorldPick = "world_channel_pick"
	// 懒改派写归属之前重读属主。
	readUnknownSiteWorldLazy = "world_channel_lazy"
	// 伸缩决策读排空标记、频道人数、冷却标记、期望频道数。
	readUnknownSiteAutoscaleDecide = "autoscale_decide"
	// 排空推进读频道属主、频道人数。
	readUnknownSiteAutoscaleDrain = "autoscale_drain"
	// 排空收尾在原子认领之前预读镜像集合。
	readUnknownSiteAutoscaleFinish = "autoscale_finish"
	// rebalance 规划读频道属主、人数、老节点的负载集成员资格:该频道不排进本轮计划。
	// 迁移改归属之前读镜像集合:这次迁移作废,回收刚建的实体。
	readUnknownSiteRebalancePlan = "rebalance_plan"
	// 场景销毁(空闲 / force)提交前读镜像源与镜像子集合:本轮不销毁,回 (false, err)。
	readUnknownSiteInstanceDestroy = "instance_destroy"
	// 镜像级联:**销毁已提交之后**重读子集合失败(退回提交前快照级联、保留索引),
	// 或读不到某个子镜像的 zone(跳过它、保留索引)。这一项不是「没写」。
	readUnknownSiteMirrorCascade = "mirror_cascade"
	// 镜像去重读已有镜像的属主:不复用也不当陈旧摘掉,照常新建。
	readUnknownSiteMirrorDedup = "mirror_dedup"
	// 孤儿频道清理第一段读属主、镜像源、排空集合:整组留到下一次 fullSync。
	readUnknownSiteOrphanCleanup = "orphan_cleanup"
	// 按用途选节点时读节点类型:这一轮当没有可用节点,调用方跳过或回可重试错误。
	readUnknownSiteNodeSelection = "node_selection"
)

// noteReadUnknown 记一次「读不到,状态未知」:计 ownership_read_unknown_total 并打 ERROR。
// what 说明读的是什么(必要时带上调用方因此怎么做),可以带 scene_id / node_id —— 它们只进日志,不进 label。
// 调用方保证不按读错的结果行事,具体是放弃还是退回快照见各 site 常量的注释。
func noteReadUnknown(zoneID uint32, site, what string, err error) {
	metrics.ObserveOwnershipReadUnknown(zoneID, site)
	logx.Errorf("[ReadUnknown] %s: failed to read %s; state unknown, not acting on it: zone=%d err=%v",
		site, what, zoneID, err)
}
