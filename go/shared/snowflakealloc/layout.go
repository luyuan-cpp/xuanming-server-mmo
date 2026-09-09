package snowflakealloc

import (
	"fmt"

	"shared/snowflake"
)

// worker 段布局:cluster / slot 的拆合是**分配器**的事,发号器不知道。
//
// shared/snowflake 只认一个 17 位 worker(NodeBits),它对 worker 里有没有子段、子段
// 怎么切一无所知 —— 这是刻意的:发号器的契约是 [time][worker][step],把部署拓扑
// (集群号)塞进去会让它跟运维布局耦合。cluster/slot 的语义从这里开始:
//
//   - cluster 是部署级常量(ConfigMap / env,运维一次性设定,策划不碰),
//     两个集群各自的 etcd 天然不撞;同一 etcd 里不同 cluster 也不撞。
//   - slot 是本包在 /snowflake/<kind>/c<cluster>/ 下申领的槽位。
//   - worker = (cluster << slotBits) | slot,合成后整体交给发号器。
//   - 存量 id 不作废:旧 id 的 worker17 实际值 ≤ 几百,恰等于新布局 cluster=0 的
//     slot;时间字段只增不减,所以只要 cluster 0 的 slot ≤ MaxSlot 就逐位不撞。
//
// 设计稿 docs/design/node-id-overhaul-plan-20260908.md §5。位宽一处定义,
// C++ 侧(snow_flake.h kClusterBits / kSlotBits)用同一组往返向量对拍。
const (
	// ClusterBits / SlotBits 是 shared/snowflake 17 位 worker 段的默认切分 [cluster5][slot12]。
	// 两者之和必须等于 snowflake.NodeBits(layout_test.go 钉住)。
	ClusterBits uint = 5
	SlotBits    uint = 12
	// MaxCluster / MaxSlot 是默认布局下两个子段的上界(含)。
	MaxCluster uint64 = (1 << ClusterBits) - 1
	MaxSlot    uint64 = (1 << SlotBits) - 1

	// PlayerIDClusterBits / PlayerIDSlotBits 是 login PlayerId 的变体:bwmarrin 的 13 位
	// node 段切成 [cluster3][slot10],worker = (cluster << 10) | slot。login 侧把这两个
	// 位宽显式传进 Options{ClusterBits, SlotBits};这里只是把布局钉成常量供对拍。
	PlayerIDClusterBits uint = 3
	PlayerIDSlotBits    uint = 10
)

// ComposeWorkerID 把 (cluster, slot) 按给定位宽合成发号器吃的 worker id:
// worker = (cluster << slotBits) | slot。任一段越界即返回错误(fail-closed,
// 越界的 cluster 会溢到时间段、越界的 slot 会溢到 cluster 段,两者都是静默撞号)。
//
// 默认布局传 ClusterBits / SlotBits;login 传 PlayerIDClusterBits / PlayerIDSlotBits。
func ComposeWorkerID(cluster uint32, slot uint64, clusterBits, slotBits uint) (uint64, error) {
	if clusterBits == 0 || slotBits == 0 || clusterBits+slotBits > 63 {
		return 0, fmt.Errorf("snowflakealloc: invalid worker layout cluster%d/slot%d", clusterBits, slotBits)
	}
	if uint64(cluster) > (uint64(1)<<clusterBits)-1 {
		return 0, fmt.Errorf("snowflakealloc: cluster %d exceeds %d bits (max %d)", cluster, clusterBits, (uint64(1)<<clusterBits)-1)
	}
	if slot > (uint64(1)<<slotBits)-1 {
		return 0, fmt.Errorf("snowflakealloc: slot %d exceeds %d bits (max %d)", slot, slotBits, (uint64(1)<<slotBits)-1)
	}
	return (uint64(cluster) << slotBits) | slot, nil
}

// DecodeWorkerID 把 worker id 按给定位宽拆回 (cluster, slot)。高于 clusterBits+slotBits
// 的位被丢弃。
func DecodeWorkerID(worker uint64, clusterBits, slotBits uint) (cluster uint32, slot uint64) {
	slotMask := (uint64(1) << slotBits) - 1
	clusterMask := (uint64(1) << clusterBits) - 1
	return uint32((worker >> slotBits) & clusterMask), worker & slotMask
}

// WorkerOf 取出 shared/snowflake 布局的 id 里完整的 17 位 worker 段(cluster 与 slot 未拆)。
func WorkerOf(id uint64) uint64 {
	return (id >> snowflake.StepBits) & snowflake.NodeMask
}

// DecodeID 把一个 shared/snowflake 布局的 id 拆成 (自 snowflake.Epoch 起的秒, cluster, slot, step),
// worker 段按默认的 ClusterBits / SlotBits 切。这是 C++ ParseGuid 的等价物,两端用同一组
// 向量对拍。只用发号器的公开常量(NodeBits / StepBits),不依赖它的内部布局。
//
// login 的 bwmarrin PlayerId 是另一种 id 布局(毫秒时间 / 13 位 node),不能用这个拆。
func DecodeID(id uint64) (epochSec uint64, cluster uint32, slot uint64, step uint64) {
	stepMask := (uint64(1) << snowflake.StepBits) - 1
	cluster, slot = DecodeWorkerID(WorkerOf(id), ClusterBits, SlotBits)
	return id >> (snowflake.NodeBits + snowflake.StepBits), cluster, slot, id & stepMask
}
