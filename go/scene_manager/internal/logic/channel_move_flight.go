package logic

import (
	"context"
	"errors"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
)

// 进场请求路径上的频道改派合并(docs/design/agones-scene-node-high-density.md §13.10)。
//
// 懒改派(lazyReassignStaleWorldChannel)与 EnterScene 的死节点自愈(resolveScene → reassignSceneOffDeadNode)都跑在
// 进场请求上:任意副本、任意并发。节点一死,它上面的玩家会一起重连、一起进同一个频道。没有合并时:
//   - 每个请求都先在同一台 GameServer 上占名额:K8s 读改写同一个对象,互相冲突、重试;
//   - 并发的占用把目标临时占满,后到的回落到 GSA,在别的节点上白建实体,CAS 落空后再拆;
//   - 最后只有一个改派 CAS 落地,K8s 写入与建拆却随请求数放大。
//
// 合并之后,同一进程里同一时刻每个频道只跑一次改派,同时到的请求等它的结果。多副本之间最多每个副本一次。
// 这不是锁:正确性仍由改派 CAS 与名额提交的比较并交换保证,这里只省重复的活。
//
// rebalance 与 init 的死节点改派不进来:前者只在领导者上跑,后者在 zone 锁内,本来就不并发;它们要的返回值
// (迁移是否落地、本轮 RPC 记账)也与进场请求不同。
//
// golang.org/x/sync/singleflight 不在本模块依赖里(离线构建拿不到新模块),这里只写进场请求用得到的最小部分。

// errChannelMoveAborted 是改派函数没有正常返回(panic)时分给等待者的结果。
var errChannelMoveAborted = errors.New("world channel move aborted")

// channelMoveFlight 是一次在跑的改派。node、err 由发起者在关闭 done 之前写好,等待者在 done 关闭之后读。
type channelMoveFlight struct {
	done chan struct{}
	// waiters 是等这一次结果的请求数,只在 channelMoveFlightsMu 下读写。
	waiters int
	node    string
	err     error
}

var (
	channelMoveFlightsMu sync.Mutex
	// channelMoveFlights 以 scene_id 为键(全局唯一,不用再带 zone)。一次改派结束就删,不会累积。
	channelMoveFlights = make(map[uint64]*channelMoveFlight)
)

// joinChannelMove 为 sceneID 跑一次 move,回它交出的落点或错误。本进程里已经有一次在跑时不再跑,等那一次的结果。
//
//   - 等待的一方按自己的 ctx 放弃等待(回 ctx.Err()),在跑的那一次不受影响。
//   - 在跑的那一次用发起者自己的 ctx:发起者被取消时,等待者拿到同一个错误,由上游重试;重试时发起一次新的。
//   - 结果原样分给所有等待者,错误值也是同一个,errors.Is 照常可用(例如 ErrReentryBarrierPending)。
func joinChannelMove(ctx context.Context, sceneID uint64, move func() (string, error)) (string, error) {
	channelMoveFlightsMu.Lock()
	if flight, running := channelMoveFlights[sceneID]; running {
		flight.waiters++
		channelMoveFlightsMu.Unlock()
		select {
		case <-flight.done:
			return flight.node, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &channelMoveFlight{done: make(chan struct{}), err: errChannelMoveAborted}
	channelMoveFlights[sceneID] = flight
	channelMoveFlightsMu.Unlock()

	defer func() {
		channelMoveFlightsMu.Lock()
		delete(channelMoveFlights, sceneID)
		waiters := flight.waiters
		channelMoveFlightsMu.Unlock()
		close(flight.done)
		if waiters > 0 {
			logx.Infof("[World] Channel %d move shared with %d concurrent requests: node=%q err=%v",
				sceneID, waiters, flight.node, flight.err)
		}
	}()
	flight.node, flight.err = move()
	return flight.node, flight.err
}
