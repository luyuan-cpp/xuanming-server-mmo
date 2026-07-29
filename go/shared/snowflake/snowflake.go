package snowflake

import (
	"fmt"
	"sync"
	"time"
)

// Layout matches C++ SnowFlake: [time:32][node:17][step:15]
// Epoch: 2026-03-14 00:00:00 UTC (1773446400)
const (
	Epoch    uint64 = 1773446400
	NodeBits uint64 = 17
	StepBits uint64 = 15

	timeShift = NodeBits + StepBits
	nodeShift = StepBits
	stepMask  = (1 << StepBits) - 1
	NodeMask  = (1 << NodeBits) - 1
)

// waitBudget 是"等真实时钟越过高水位秒"的上限。超时后改为借下一个逻辑秒继续发号,
// 绝不把 lastTime 往回写。3s 与 C++ 侧 (snow_flake.h WaitNextTime) 保持一致。
const waitBudget = 3 * time.Second

// Node generates unique 64-bit IDs using the Snowflake algorithm.
// Thread-safe via mutex.
type Node struct {
	mu       sync.Mutex
	nodeID   uint64
	lastTime uint64
	step     uint64
}

// NewNode 创建一个 SnowFlake 生成器,并施加**启动 guard**:把 lastTime 置为当前秒、
// step 置满,于是第一个 ID 一定落在下一秒。
//
// 这不是保守,是必需的:
//   - worker id / node_id 的回收是"上一任进程退出即释放" —— internal/node.Close()
//     与 snowflakealloc.Handle.Close() 都会立刻 Delete key + Revoke lease;
//   - snowflakealloc 还有 hostname 亲和,同一台机器上重启**必然**拿回同一个 worker id;
//   - 本包是秒级时间戳,新进程从 step=0 重新开始。
//
// 三者叠加的结果是:进程在同一日历秒内重启,新老进程发出来的号**逐位相同**。
// C++ 侧一直用 SnowFlakeGuard 挡这个窗口(见 etcd_service.cpp ActivateSnowFlakeAfterGuard),
// Go 侧此前没有任何防护 —— 见 docs/design/snowflake-node-id-lease-recycling.md 里
// "Go lacks the SnowFlakeGuard mechanism" 那条,当时被判成"hostname key 就是 guard",
// 但 hostname 亲和恰恰保证了重启后拿到**同一个** worker id,反而让撞号成为必然而非偶然。
//
// 代价:本进程第一个 ID 最多晚 1 秒发出。
//
// Panics if nodeID exceeds 17-bit range (0..131071).
func NewNode(nodeID uint64) *Node {
	if nodeID > NodeMask {
		panic(fmt.Sprintf("snowflake: node ID %d exceeds max %d", nodeID, NodeMask))
	}
	return &Node{
		nodeID:   nodeID,
		lastTime: nowEpoch(),
		step:     stepMask,
	}
}

// Generate produces a globally unique uint64 ID.
//
// lastTime 是**高水位**,任何情况下都不回退 —— 回退就会把已经发出去的 (秒, step)
// 组合再发一遍。时钟回拨只影响 ID 里时间字段有多贴近真实时间,不影响唯一性与单调性。
func (n *Node) Generate() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := nowEpoch()

	switch {
	case now > n.lastTime:
		n.lastTime = now
		n.step = 0
	case n.step < stepMask:
		// 同一秒,或者时钟回拨:继续消费高水位这一秒剩下的 step 池。
		// 回拨时刻意**不**自旋等待 —— 旧实现会握着 n.mu 把整个服务卡住整个回拨幅度。
		n.step++
	default:
		// 高水位这一秒的 32768 个号发完了,只能等真实时钟越过它。有界等待;
		// 等不到就借下一个逻辑秒,唯一性优先于时间字段的精度。
		advanced := n.waitNextTime(n.lastTime)
		if advanced > n.lastTime {
			n.lastTime = advanced
		} else {
			n.lastTime++
		}
		n.step = 0
	}

	return (n.lastTime << timeShift) |
		(n.nodeID << nodeShift) |
		n.step
}

// waitNextTime 有界等待真实时钟越过 last。返回值可能仍 <= last(时钟停摆 / 大幅回拨),
// 调用方必须自己保证不把 lastTime 往回写。
func (n *Node) waitNextTime(last uint64) uint64 {
	deadline := time.Now().Add(waitBudget)
	for {
		now := nowEpoch()
		if now > last {
			return now
		}
		if time.Now().After(deadline) {
			return now
		}
		time.Sleep(time.Millisecond)
	}
}

// nowEpoch 返回自 Epoch 起的秒数。
//
// 系统时钟早于 Epoch(容器时钟没同步、回到 1970)时,uint64 减法会下溢成一个天文数字,
// 把所有 ID 的时间段顶到远未来且再也不会推进。这里钳到 0,让 Generate 走"回拨"分支
// 从高水位继续发号。
// 抽成包级变量只为在测试里注入虚拟时钟(绕开 32768 ID/s 的真实容量墙、
// 以及无法真的把系统时钟往回拨这两个限制);生产路径恒等于下面这个实现。
var nowEpoch = func() uint64 {
	sec := time.Now().Unix()
	if sec < int64(Epoch) {
		return 0
	}
	return uint64(sec) - Epoch
}
