package loginqueue

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// drainMonitorPoint 是排空判定循环的 safego 点位名。
// 它会直接变成 Prometheus label,所以必须是常量,不能拼运行期值。
const drainMonitorPoint = "login.gate_drain_monitor"

// gate 排空的第 2/3 步:等在线掉下去,然后判定"现在缩容是安全的"。
//
// **执行器自动化的是判定,不是踢人。** 这一条是刻意的,理由有两个:
//
//  1. 那台 gate 上的玩家最终靠"Pod 下线 -> 客户端重连 -> PickGate 已排除
//     draining gate -> 落到别的 gate"完成改派 —— 这条链现在就通,不需要
//     服务端主动踢。主动踢只是把同一件事提前,并不改变玩家体验。
//  2. 现有的踢人原语 `KickPlayerEvent` 会让客户端弹
//     `kLoginBeKickByAnOtherAccount`(账号在别处登录)。给一个正在做计划内
//     缩容的玩家看这条提示是**误导**,比干净断开更糟。要正确地踢,得先加一个
//     "服务器维护,正在为你切换接入点"的 tip,那是 proto + 表的改动。
//
// 所以这里的产出是一个明确的信号:`gate:{id}:drained`。缩容脚本 / 运维
// 看到它才动手删 Pod。什么时候可以牺牲最后那批玩家的连接仍然是人的决定,
// 只是"够不够安全"这个判断被自动化了。

const (
	// GateDrainedKeyFmt 标记某台 gate 已排空到可以安全缩容。
	// 值是判定理由,便于运维知道是"人走干净了"还是"等超时了"。
	GateDrainedKeyFmt = "gate:%d:drained"

	// DrainReasonBelowThreshold: 在线数已经掉到阈值以下,缩容影响很小。
	DrainReasonBelowThreshold = "below_threshold"
	// DrainReasonDeadline: 等到期了,还剩人。缩容会让这批人断线重连。
	DrainReasonDeadline = "deadline"
)

// go-redis 对 TTL 的两个特殊回复不乘精度,原样返回 Duration(-2) / Duration(-1)。
const (
	redisTTLKeyMissing time.Duration = -2 // 键不存在
	redisTTLNoExpiry   time.Duration = -1 // 键存在但没有过期时间
)

func gateDrainedKey(nodeID uint32) string {
	return "gate:" + strconv.FormatUint(uint64(nodeID), 10) + ":drained"
}

// GateDrainPolicy 是排空判定的参数。
type GateDrainPolicy struct {
	// DrainedBelowPlayers: 在线数 <= 这个值就算排空。0 表示必须一个人都不剩。
	DrainedBelowPlayers uint32
	// DeadlineSeconds: 从标记 draining 起算,等这么久之后无论还剩多少人都
	// 判定为可缩容。0 表示**永不**因超时放行 —— 只认人走干净,
	// 适合"绝不主动断玩家"的运营口径。
	DeadlineSeconds int64
}

// GateDrainState 是一台 gate 的排空判定结果。
type GateDrainState struct {
	NodeID    uint32
	Online    uint32
	Drained   bool
	Reason    string
	WaitedSec int64
}

// EvaluateGateDrain 判断一台已标记 draining 的 gate 是否可以安全缩容。
//
// 纯函数,不碰 Redis —— 判定逻辑是这套机制里唯一有分支的部分,
// 单独拎出来才能覆盖到边界(阈值相等、deadline=0 永不放行、时钟回拨)。
func EvaluateGateDrain(online uint32, markedAtUnix, nowUnix int64, policy GateDrainPolicy) GateDrainState {
	waited := nowUnix - markedAtUnix
	if waited < 0 {
		// 时钟回拨 / 标记时间来自别的机器。当作刚标记,宁可多等一会儿,
		// 也不要因为一个负数直接把人断了。
		waited = 0
	}

	st := GateDrainState{NodeID: 0, Online: online, WaitedSec: waited}

	if online <= policy.DrainedBelowPlayers {
		st.Drained = true
		st.Reason = DrainReasonBelowThreshold
		return st
	}

	// DeadlineSeconds<=0 表示永不超时放行:只认人走干净。
	if policy.DeadlineSeconds > 0 && waited >= policy.DeadlineSeconds {
		st.Drained = true
		st.Reason = DrainReasonDeadline
		return st
	}

	return st
}

// GateOnline 是监控循环需要的最小 gate 快照。
type GateOnline struct {
	NodeID      uint32
	PlayerCount uint32
}

// GateSnapshotFunc 返回当前所有 gate 的在线快照(**不要**过滤 draining ——
// 正在排空的那些才是这里要看的)。
type GateSnapshotFunc func(ctx context.Context) ([]GateOnline, error)

// EvaluateDrainingGates 跑一轮判定,把已排空的 gate 写上 drained 标记。
// 返回本轮判定为可缩容的 gate。导出是为了让单测直接驱动一轮,不用等 ticker。
func EvaluateDrainingGates(
	ctx context.Context,
	rdb *redis.Client,
	gates []GateOnline,
	policy GateDrainPolicy,
	nowUnix int64,
) []GateDrainState {
	if rdb == nil || len(gates) == 0 {
		return nil
	}

	var drained []GateDrainState
	for _, g := range gates {
		markedAtRaw, err := rdb.Get(ctx, gateDrainingKey(g.NodeID)).Result()
		if err != nil || markedAtRaw == "" {
			// 没在排空。顺手清掉可能残留的 drained 标记 —— 运维取消缩容后
			// 不清的话,下次这台 gate 会被误判成"随时可以删"。
			rdb.Del(ctx, gateDrainedKey(g.NodeID))
			continue
		}
		markedAt, _ := strconv.ParseInt(markedAtRaw, 10, 64)

		st := EvaluateGateDrain(g.PlayerCount, markedAt, nowUnix, policy)
		st.NodeID = g.NodeID
		if !st.Drained {
			logx.Infof("[GateDrain] gate %d draining: %d player(s) still online, waited %ds",
				g.NodeID, g.PlayerCount, st.WaitedSec)
			continue
		}

		// drained 标记跟着 draining 标记同寿:draining 一过期,这台 gate 就
		// 重新接客了,drained 不能比它活得久。所以 drained 的 TTL 只能取 draining 的
		// 剩余 TTL;拿不到正值就跳过本轮写入,**不能**回落成固定时长补写 —— 那样 drained
		// 会比 draining 活得久,之后同一 node_id 再标 draining 时,残留的 drained 会被读成
		// "已排空"。
		//
		// 仍有一个窗口:读到正的剩余 TTL 之后、SET 之前 draining 被删,drained 会带着读到的
		// 剩余 TTL 落下。该 node_id 仍在快照里时,下一轮走上面"没在排空"分支清掉;不在时最多
		// 活到那个剩余 TTL,不超过原 draining 的寿命。k8s_gate_drain.ps1 标记前先 DEL drained,
		// 且每轮与 draining 一起 MGET,draining 不在就中止,不会据这条残留删 Pod。
		ttl, err := rdb.TTL(ctx, gateDrainingKey(g.NodeID)).Result()
		switch {
		case err != nil:
			// 查询失败:下一轮再判。排空判定晚一个周期无害,补写一个猜的 TTL 有害。
			logx.Errorf("[GateDrain] gate %d: read draining TTL failed, skip marking drained this round: %v",
				g.NodeID, err)
			continue
		case ttl == redisTTLKeyMissing:
			// draining 在本轮 GET 之后被删(脚本清标记 / 运维取消)或恰好过期:gate 已重新接客,
			// 按"没在排空"处理,同样顺手清掉残留的 drained。
			rdb.Del(ctx, gateDrainedKey(g.NodeID))
			logx.Infof("[GateDrain] gate %d: draining mark vanished during evaluation, not marking drained",
				g.NodeID)
			continue
		case ttl == redisTTLNoExpiry:
			// draining 必须带 TTL(见 GateDrainingKeyFmt):不带就是有人手工 SET 漏了 EX,这台
			// gate 会被永久排除。drained 跟着永不过期只会放大误操作,所以不写,打 ERROR 让人处理。
			logx.Errorf("[GateDrain] gate %d: %s has no TTL (it must expire), not marking drained — "+
				"set a TTL on it or DEL it to cancel the drain", g.NodeID, gateDrainingKey(g.NodeID))
			continue
		case ttl <= 0:
			// 剩余 TTL 按秒取整后为 0:draining 马上过期,写 drained 没有意义;
			// 而且 go-redis 的 Set 过期传 0 表示永不过期,绝不能落到下面。
			continue
		}
		if err := rdb.Set(ctx, gateDrainedKey(g.NodeID), st.Reason, ttl).Err(); err != nil {
			logx.Errorf("[GateDrain] gate %d: failed to mark drained: %v", g.NodeID, err)
			continue
		}

		if st.Reason == DrainReasonDeadline {
			// 到期放行意味着还剩 st.Online 个玩家会被缩容断线。这不是常规
			// 事件,必须显式可见,不能淹在 INFO 里。
			logx.Errorf("[GateDrain] gate %d marked DRAINED by deadline after %ds with %d player(s) "+
				"still online — scaling it in will disconnect them",
				g.NodeID, st.WaitedSec, st.Online)
		} else {
			logx.Infof("[GateDrain] gate %d marked DRAINED (%d online <= %d), safe to scale in",
				g.NodeID, st.Online, policy.DrainedBelowPlayers)
		}
		drained = append(drained, st)
	}
	return drained
}

// StartGateDrainMonitor 起一个周期性排空判定循环。
// interval<=0 或 snapshot==nil 时直接返回,不起 goroutine。
func StartGateDrainMonitor(
	ctx context.Context,
	rdb *redis.Client,
	snapshot GateSnapshotFunc,
	policy GateDrainPolicy,
	interval time.Duration,
) {
	if rdb == nil || snapshot == nil || interval <= 0 {
		logx.Info("[GateDrain] monitor disabled")
		return
	}

	// safego 而不是裸 go func:排空判定炸一轮不该带走整个 login 进程,
	// 也不该让循环从此静默停摆(那会让缩容中的 gate 永远不被标记)。
	safego.Go(drainMonitorPoint, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logx.Infof("[GateDrain] monitor started: interval=%s drained_below=%d deadline=%ds",
			interval, policy.DrainedBelowPlayers, policy.DeadlineSeconds)

		for {
			select {
			case <-ctx.Done():
				logx.Info("[GateDrain] monitor stopped")
				return
			case <-ticker.C:
				safego.Run(drainMonitorPoint, func() {
					gates, err := snapshot(ctx)
					if err != nil {
						logx.Errorf("[GateDrain] gate snapshot failed: %v", err)
						return
					}
					EvaluateDrainingGates(ctx, rdb, gates, policy, time.Now().Unix())
				})
			}
		}
	})
}
