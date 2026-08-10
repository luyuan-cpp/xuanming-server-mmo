package snowflake

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
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
// maxBorrowAheadSec 是借位预算:ID 时间字段最多允许超前墙钟这么多秒。
// 借下一个逻辑秒是容量墙/时钟停摆下保唯一性的正常手段,但无上限的借位会让
// ID 时间字段越漂越远 —— 一旦进程在深度借位中崩溃,持久水位若没追平,
// 继任者的撞号窗口就有多深(见 snowflakealloc.advanceGuard);且时间字段
// 本身也会失去参考价值。超出预算 Generate 直接返回 ErrBorrowLimitExceeded,
// fail-closed 交给调用方:墙钟以 1s/s 追赶,预算内的超前会自愈,错误是暂态的、
// 可重试的(与 ErrFenced 的永久性不同)。
const maxBorrowAheadSec = 10

// ErrBorrowLimitExceeded 表示发号器已把逻辑秒借到墙钟前面 maxBorrowAheadSec 秒,
// 本次发号被拒。这是**暂态**错误:等墙钟追上来(最多 maxBorrowAheadSec 秒)即恢复。
// 调用方与 ErrFenced 同样处理 —— 这次操作整体失败,不得用 0 或自造 id 继续。
var ErrBorrowLimitExceeded = errors.New(
	"snowflake: logical second borrowed too far ahead of wall clock; retry after the clock catches up")

// ErrFenced 表示本进程已经失去 worker id 的所有权,发号器被永久停用。
// 调用方必须把它当成"这次操作做不了",fail-closed 返回错误,**不得**降级成 0 或自己编一个 id。
var ErrFenced = errors.New("snowflake: node fenced (worker id lease lost); refusing to mint")

type Node struct {
	mu       sync.Mutex
	nodeID   uint64
	lastTime uint64
	step     uint64
	// lastBorrowRejectLogSec 限制借位预算耗尽的 ERROR 日志为每墙钟秒一条
	// (持续过载时 Generate 每次调用都会走到拒绝分支)。mu 保护下读写。
	lastBorrowRejectLogSec uint64

	// fenced 是失租后的硬闸。用 atomic 是为了让 Fence() 能从 keepalive 的 goroutine 调,
	// 而不必跟发号路径抢 mu(失租时发号可能正阻塞在 waitNextTime 里)。
	fenced atomic.Bool

	// bootGuardPending 表示 NewNode 的启动 guard 刚把 step 池置满、且尚未被首个
	// Generate() 消化。只用于抑制"guard 造成的首次耗尽"的误报,不影响发号语义。
	bootGuardPending bool
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
	// 墙钟早于 Epoch(容器时钟没同步 / 快照恢复回到 1970)时 nowEpoch() 恒返回 0,
	// 于是 lastTime=0、step 满 ⇒ 首个 Generate 借位到逻辑秒 1,**每次重启都走完全相同的
	// 确定性序列**,同 worker id 的新老进程逐位重号且全程零日志。
	// 这里 fail-closed —— 与 C++ SetGuardTime 对 guard<epoch 的处理同口径
	// (snow_flake.h:拒绝写入并报错),宁可起不来也不发错号。
	if sec := time.Now().Unix(); sec < int64(Epoch) {
		panic(fmt.Sprintf("snowflake: wall clock %d is before epoch %d; refusing to mint "+
			"(every restart would replay the identical ID sequence) — fix NTP / container clock first",
			sec, Epoch))
	}
	return &Node{
		nodeID:   nodeID,
		lastTime: nowEpoch(),
		step:     stepMask,
		// 上面这次"step 池置满"是 guard 有意为之,不是撞到容量墙。
		// 标记一下,让紧随其后的第一次 Generate() 不要误报成容量告警。
		bootGuardPending: true,
	}
}

// Generate produces a globally unique uint64 ID.
//
// lastTime 是**高水位**,任何情况下都不回退 —— 回退就会把已经发出去的 (秒, step)
// 组合再发一遍。时钟回拨只影响 ID 里时间字段有多贴近真实时间,不影响唯一性与单调性。
// SetGuardTime 把发号起点抬到 guardEpochSec(自 Epoch 起的秒),**地板语义**:
// 只在它比当前高水位更晚时才生效,绝不把高水位往回写。
//
// 为什么需要它:NewNode 的启动 guard 只是"绝不在构造那一秒发号"的**点排除**,
// 它以本机墙钟为基准,顶不住"前任高水位 > 本机当前秒"这一类情况 ——
// 跨机接管时新持有者时钟落后于前任、本机时钟被 NTP 回拨、或前任因发满 step 池
// 借过逻辑秒(高水位跑到墙钟前面),三者都会让新进程重发前任已发出的号。
// 把前任写在 etcd 里的高水位当地板注入,这些情况一并消失。
// 这是 C++ 侧 SnowFlake::SetGuardTime 的等价物(见 cpp/.../snow_flake.h)。
//
// 必须在任何 Generate 之前调用。
func (n *Node) SetGuardTime(guardEpochSec uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if guardEpochSec <= n.lastTime {
		return
	}
	n.lastTime = guardEpochSec
	// 把这一秒的 step 池置满,于是下一个号必然落在 guard 之后的某一秒。
	n.step = stepMask
	// 与 NewNode 的启动 guard 同理:这次"耗尽"是设计动作,不是容量问题,不该报容量告警。
	n.bootGuardPending = true
}

// HighWaterEpochSec 返回当前发号高水位所在的逻辑秒(自 Epoch 起)。
//
// 供 snowflakealloc 的持久水位使用:水位的契约是"不早于最后一次发号的秒",
// 而借位(step 耗尽 / 时钟停摆时 lastTime 跑到墙钟前面)会让墙钟低于真实
// 高水位 —— 只持久化墙钟就关不上"前任借过逻辑秒"的重号窗口。
func (n *Node) HighWaterEpochSec() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastTime
}

// Fence 永久停用本发号器。失去 worker id 的 etcd 租约后必须立刻调用:
// 此刻另一个进程可能已经拿着同一个 worker id 在发号,再发一个就是确定性撞号。
//
// 为什么不能只靠"收到 Lost() 就停服":go-zero 的 zrpc.RpcServer.Stop() 实测
// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),**完全不碰 gRPC server** ——
// 调它既不拒新请求也不排空在途,服务照常接客、照常发号。所以正确性必须由发号器
// 自己 fail-closed 兜住,不能寄托在任何"停服"语义上。幂等。
func (n *Node) Fence() { n.fenced.Store(true) }

// IsFenced 供调用方在昂贵操作前提前判断,避免做完一堆活才发现发不了号。
func (n *Node) IsFenced() bool { return n.fenced.Load() }

// Generate 产生一个全局唯一 ID。
//
// 返回 ErrFenced 表示本进程已失去 worker id 所有权,**这次操作必须整体失败**。
// 调用方不得把错误吞掉后用 0 或自造 id 继续 —— 那正是撞号要防的东西。
func (n *Node) Generate() (uint64, error) {
	// 先查闸再上锁:失租时发号路径可能正阻塞在 waitNextTime 里,不该再排队等锁。
	if n.fenced.Load() {
		return 0, ErrFenced
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	// 上锁后复查:Fence() 可能发生在上面那次检查与拿到锁之间。
	if n.fenced.Load() {
		return 0, ErrFenced
	}

	now := nowEpoch()

	// guard 预置只影响**紧随其后的第一次**发号,所以标记由第一次 Generate 无条件消化 ——
	// 不能只在耗尽分支里清:若时钟在首次发号前就已跨秒,这里走的是"换新秒"分支,
	// 标记会一直留着,把之后一次**真实**的容量耗尽静音掉。
	guardPending := n.bootGuardPending
	n.bootGuardPending = false

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
		//
		// 耗尽本身必须记 ERROR:它意味着本节点已撞到 32768/s 的容量墙,或处在时钟回拨
		// 窗口内(回拨期间整个窗口共享同一个 step 池,是耗尽的主要放大因素)。
		// behind>0 即可判定是回拨叠加而非纯粹量大,两种成因的处置完全不同。
		// 不必额外限频:同一个逻辑秒不可能耗尽两次,且这里持有 n.mu,天然 ≤1 条/逻辑秒。
		//
		// ⚠️ 唯一例外是 NewNode 的启动 guard 预置的那一次:guard 特意把 step 池置满,
		// 好让首个号必然落在构造秒之后。那次"耗尽"是设计动作而非容量问题,**每个进程
		// 启动都会发生**;若也报 ERROR,运维会被训练成忽略这条告警,真撞容量墙时反而看不见。
		if !guardPending {
			behind := uint64(0)
			if now < n.lastTime {
				behind = n.lastTime - now
			}
			logx.Errorf("[snowflake] step pool exhausted: worker_id=%d logical_second=%d cap=%d clock_behind=%ds; "+
				"waiting up to %v for the clock, then borrowing the next logical second",
				n.nodeID, n.lastTime, stepMask+1, behind, waitBudget)
		}

		advanced := n.waitNextTime(n.lastTime)
		if advanced > n.lastTime {
			n.lastTime = advanced
		} else {
			// 等不到真实时钟:借位。此时 ID 的时间段会超前真实时钟。
			// waitNextTime 的返回值就是最后一次观测的墙钟秒,直接拿它做预算判定。
			//
			// 超出借位预算则拒绝发号(见 maxBorrowAheadSec 注释)。注意这也覆盖
			// guard 注入的超前:接管时前任高水位(SetGuardTime)比本机墙钟快超过
			// 预算的话,这里会 fail-closed 到墙钟追进预算圈内为止 —— 唯一性优先,
			// 且绝不把 lastTime 借得比水位能追平的速度还快。
			if n.lastTime+1 > advanced+maxBorrowAheadSec {
				// 限频:同一墙钟秒只喊一次,免得持续过载时每次调用都刷 ERROR。
				if n.lastBorrowRejectLogSec != advanced {
					n.lastBorrowRejectLogSec = advanced
					logx.Errorf("[snowflake] borrow budget exhausted: worker_id=%d high_water=%d wall_clock=%d "+
						"ahead_limit=%ds; refusing to mint until the clock catches up",
						n.nodeID, n.lastTime, advanced, maxBorrowAheadSec)
				}
				return 0, ErrBorrowLimitExceeded
			}
			logx.Errorf("[snowflake] clock did not advance within %v: worker_id=%d borrowing logical second %d "+
				"(ID time field now runs ahead of the wall clock)", waitBudget, n.nodeID, n.lastTime+1)
			n.lastTime++
		}
		n.step = 0
	}

	// waitNextTime 可能阻塞数秒(时钟停摆),期间可能已经失租。此时绝不能把号交出去:
	// 另一个进程可能已经拿着同一个 worker id 在发号了。
	if n.fenced.Load() {
		return 0, ErrFenced
	}

	return (n.lastTime << timeShift) |
		(n.nodeID << nodeShift) |
		n.step, nil
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
// NowEpochSec 返回当前时刻自 Epoch 起的秒数,供分配器写持久水位用
// (水位与发号器的时间段必须同一基准,否则地板会错位)。
func NowEpochSec() uint64 { return nowEpoch() }

var nowEpoch = func() uint64 {
	sec := time.Now().Unix()
	if sec < int64(Epoch) {
		return 0
	}
	return uint64(sec) - Epoch
}
