package logic

import (
	"slices"

	"scene_manager/internal/svc"
)

// 一个 scene_manager 进程只编排一个 zone(docs/design/control-plane-zone-scoping-20261105.md §3.3、§4.3)。
//
// 为什么恒为配置的 ZoneId,而不是「etcd 快照里出现的 zone」或「Redis 里登记过的 zone」:
//   - 选主锁按 zone(scene_manager:leader:lock:zone:<z>),本副本只可能是自己 zone 的领导者;
//   - 控制域按 zone 组分实例之后,同组几个 zone 的键在同一个实例上、各自有领导者。任何「从 Redis 里发现有哪些 zone」
//     的循环都会扫到同组别的 zone,而本副本的 etcd 镜像里没有那些 zone 的节点:清扫会把它们的活节点当成陈旧成员,
//     写 death_at、摘出负载集。zone 登记集合 scene_manager:zones 因此删除;
//   - 本 zone 一个节点都没有时照样要扫:最后一个节点死后留下的负载集成员与孤儿实例只能由本 zone 的领导者收。
//
// 领导者的所有循环(补频道、rebalance、死节点清扫与收尾、空闲回收、自动扩缩容、分线目录、神秘商人、成员对账、
// 孤儿频道清理、Agones 对账)都只处理这里返回的 zone。
func orchestratedZones(svcCtx *svc.ServiceContext) []uint32 {
	return []uint32{svcCtx.Config.ZoneId}
}

// ownsControlZone 报告 zone 是不是本进程的 zone,也就是本进程能不能读写它的控制域键(§4.3 的不变量)。
// 数据面上凡是可能带别的 zone 的入口(位置落在别的 zone、跨 zone 重定向的目标)都要先判它:别的 zone 的
// 控制域键由那个 zone 的 scene_manager 维护,哪怕同组、在同一个 Redis 实例上读得到也不读。
func ownsControlZone(svcCtx *svc.ServiceContext, zoneID uint32) bool {
	return zoneID != 0 && zoneID == svcCtx.Config.ZoneId
}

// unionZones 合并若干 zone 列表,去重后升序返回。顺序确定,日志与测试才可复现。
// (原在 zone_registry.go;zone 登记集合 scene_manager:zones 2026-11-05 删除,这个工具函数搬到这里。)
func unionZones(lists ...[]uint32) []uint32 {
	set := make(map[uint32]struct{})
	for _, list := range lists {
		for _, z := range list {
			set[z] = struct{}{}
		}
	}
	out := make([]uint32, 0, len(set))
	for z := range set {
		out = append(out, z)
	}
	slices.Sort(out)
	return out
}
