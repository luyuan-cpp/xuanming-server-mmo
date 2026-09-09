package svc

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/zeromicro/go-zero/core/logx"

	sfshared "shared/snowflake"
)

// ErrPlayerIDGenFenced 表示本进程已失去 worker id 的所有权,PlayerId 发号器被永久停用。
// 调用方必须让本次操作整体失败,**不得**吞掉错误后自造一个 id。
var ErrPlayerIDGenFenced = errors.New("login: player id generator fenced (worker id lease lost)")

// ErrPlayerIDGenWatermarkStale 表示距上次水位写成功已超过自 fence 期限 F,发号器暂停发号。
// 与 ErrPlayerIDGenFenced 不同,它是**暂态**的:etcd 恢复、下一次水位写成功即自愈。
// 包装 shared/snowflake.ErrWatermarkStale,errors.Is 两者都成立。
var ErrPlayerIDGenWatermarkStale = fmt.Errorf("login: player id generator paused: %w", sfshared.ErrWatermarkStale)

// PlayerIDGen 是 PlayerId 发号器,在 bwmarrin/snowflake 外面补一个 fence 闸。
//
// 为什么要包一层:
//   - PlayerId 用的是 bwmarrin 布局(13-bit node / 9-bit step / **毫秒** epoch),
//     与 shared/snowflake 完全不同。换布局会作废全部存量 PlayerId,不能换,
//     所以不能直接复用 shared/snowflake 的 Fence();
//   - 而 bwmarrin.Node 自己没有任何"停止发号"的能力,失去 worker id 之后照发不误;
//   - 又不能指望"收到失租就停服"兜底 —— go-zero 的 zrpc.RpcServer.Stop() 实测
//     (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途。
//
// 所以正确性只能落在发号器本身:失去所有权 → Fence() → 之后 Generate 一律失败;
// 水位太久没写成功 → Generate 暂时失败(与 shared/snowflake.Node 共用同一个 FenceClock,
// 两种发号器执行同一条规则,见 sfshared.FenceClock)。
type PlayerIDGen struct {
	node   *snowflake.Node
	fenced atomic.Bool
	// fenceClock 是按水位年龄自 fence 的闸(由 snowflakealloc.Handle 持有并 Ack)。
	// nil = 不启用(单测 / 未接分配器)。
	fenceClock atomic.Pointer[sfshared.FenceClock]
	// lastStaleLogSec 限制水位过期拒发的 ERROR 日志为每墙钟秒一条。
	lastStaleLogSec atomic.Int64
	// anchor 是与 node 几乎同刻捕获的 time.Now()(带单调读数)。
	// bwmarrin 构造后内部时间 = 构造时刻 + 单调流逝,墙钟被回拨也照走 ——
	// 所以"发号器现在到几点了"不能看墙钟,要看 NowUnixMs()。
	anchor time.Time
}

// NewPlayerIDGen 包装一个已构造好的 bwmarrin 节点。
func NewPlayerIDGen(node *snowflake.Node) *PlayerIDGen {
	return &PlayerIDGen{node: node, anchor: time.Now()}
}

// NowUnixMs 返回发号器时钟口径下的当前 Unix 毫秒:锚点墙钟 + 单调流逝。
// 它是"到此刻为止可能已发出的最大时间戳"的上界(±µs 级捕获误差,由水位前推量
// 覆盖),供毫秒级持久水位使用;直接读 time.Now().UnixMilli() 在墙钟被回拨后
// 会低于发号器实际用的时间,当水位就关不住重放窗口。
func (g *PlayerIDGen) NowUnixMs() uint64 {
	return uint64(g.anchor.UnixMilli()) + uint64(time.Since(g.anchor).Milliseconds())
}

// Fence 永久停用发号器。失去 worker id 的所有权后必须立刻调用:
// 此刻另一个进程可能已经拿着同一个 worker id 在发号,再发一个就是确定性撞号
// (bwmarrin 在新毫秒把 step 归零,两个进程同毫秒的首个号逐位相同)。幂等。
func (g *PlayerIDGen) Fence() { g.fenced.Store(true) }

// IsFenced 供调用方提前判断。
func (g *PlayerIDGen) IsFenced() bool { return g.fenced.Load() }

// SetFenceClock 挂上按水位年龄自 fence 的闸;传 nil 摘掉。
func (g *PlayerIDGen) SetFenceClock(fc *sfshared.FenceClock) { g.fenceClock.Store(fc) }

// Generate 产生一个 PlayerId。fence 之后一律返回 ErrPlayerIDGenFenced;
// 水位年龄超过 F 返回 ErrPlayerIDGenWatermarkStale(暂态)。
func (g *PlayerIDGen) Generate() (int64, error) {
	if g.fenced.Load() {
		return 0, ErrPlayerIDGenFenced
	}
	if fc := g.fenceClock.Load(); fc != nil {
		if stale, age := fc.Stale(); stale {
			if sec := time.Now().Unix(); g.lastStaleLogSec.Swap(sec) != sec {
				logx.Errorf("[login] PlayerId watermark ack is %v old (budget %v): cannot prove this process still "+
					"owns its slot — refusing to mint until the next successful watermark write",
					age.Truncate(time.Second), fc.Budget())
			}
			return 0, ErrPlayerIDGenWatermarkStale
		}
	}
	if g.node == nil {
		return 0, errors.New("login: player id generator not initialised (SetNodeId not called)")
	}
	return int64(g.node.Generate()), nil
}
