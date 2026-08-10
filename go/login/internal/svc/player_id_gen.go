package svc

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/snowflake"
)

// ErrPlayerIDGenFenced 表示本进程已失去 worker id 的所有权,PlayerId 发号器被永久停用。
// 调用方必须让本次操作整体失败,**不得**吞掉错误后自造一个 id。
var ErrPlayerIDGenFenced = errors.New("login: player id generator fenced (worker id lease lost)")

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
// 所以正确性只能落在发号器本身:失租 → Fence() → 之后 Generate 一律失败。
type PlayerIDGen struct {
	node   *snowflake.Node
	fenced atomic.Bool
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

// Fence 永久停用发号器。失去 worker id 的 etcd 租约后必须立刻调用:
// 此刻另一个进程可能已经拿着同一个 worker id 在发号,再发一个就是确定性撞号
// (bwmarrin 在新毫秒把 step 归零,两个进程同毫秒的首个号逐位相同)。幂等。
func (g *PlayerIDGen) Fence() { g.fenced.Store(true) }

// IsFenced 供调用方提前判断。
func (g *PlayerIDGen) IsFenced() bool { return g.fenced.Load() }

// Generate 产生一个 PlayerId。fence 之后一律返回 ErrPlayerIDGenFenced。
func (g *PlayerIDGen) Generate() (int64, error) {
	if g.fenced.Load() {
		return 0, ErrPlayerIDGenFenced
	}
	if g.node == nil {
		return 0, errors.New("login: player id generator not initialised (SetNodeId not called)")
	}
	return int64(g.node.Generate()), nil
}
