package loginqueue

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// gate 缩容前的排空(drain)。
//
// 为什么不能直接缩:gate 持的是玩家的长连接。直接把 Pod 缩掉,那台 gate 上
// 的玩家全部断线,要靠客户端重连才能恢复 —— 这违反"不卡玩家"的底线,而且
// 缩容是**计划内**动作,没有理由让玩家付这个代价。
//
// 三步:
//  1. 标记 draining —— 新玩家不再被分配到这台 gate(本文件负责这一步)。
//  2. 等在线自然掉到阈值以下,或等到超时。
//  3. 到期把剩余玩家改派到别的 gate,然后才允许缩容。
//
// 本文件只实现第 1 步与状态读写。第 2/3 步由运维流程驱动 —— 刻意不做成
// "到期自动强踢":什么时候可以牺牲最后那批玩家的连接是运营决策,不该由一个
// 后台循环替人做。

// GateDrainingKeyFmt 标记某台 gate 正在排空。值是标记时间(Unix 秒),
// 便于运维看它排了多久。带 TTL,防止标记的人挂了之后 gate 永久不接客。
const GateDrainingKeyFmt = "gate:%d:draining"

func gateDrainingKey(nodeID uint32) string {
	return fmt.Sprintf(GateDrainingKeyFmt, nodeID)
}

// MarkGateDraining 把一台 gate 标成排空中。
//
// ttlSeconds 到期后标记自动消失、gate 重新参与分配 —— 这是有意的 fail-safe:
// 标记的人中途挂了也不会让这部分容量永久蒸发。
func MarkGateDraining(ctx context.Context, rdb *redis.Client, nodeID uint32, nowUnix int64, ttlSeconds int) error {
	if rdb == nil {
		return fmt.Errorf("gate drain: nil redis client")
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 3600
	}
	return rdb.Set(ctx, gateDrainingKey(nodeID),
		strconv.FormatInt(nowUnix, 10),
		time.Duration(ttlSeconds)*time.Second).Err()
}

// ClearGateDraining 取消排空标记(取消缩容 / 排空完成)。
func ClearGateDraining(ctx context.Context, rdb *redis.Client, nodeID uint32) error {
	if rdb == nil {
		return fmt.Errorf("gate drain: nil redis client")
	}
	return rdb.Del(ctx, gateDrainingKey(nodeID)).Err()
}

// DrainingGates 批量查询候选集里哪些 gate 正在排空。
//
// 一次 MGET 问完而不是逐个查:分配是登录热路径,N 次串行 Redis 往返会直接
// 体现在登录延迟上(压测口径见 CLAUDE.md §6)。
//
// 查询失败时返回 nil(视作"没人在排空")而不是报错:见 FilterDrainingGates
// 的注释,这条路径的失败方向必须是**放行**。
func DrainingGates(ctx context.Context, rdb *redis.Client, candidates []GateCandidate) map[uint32]bool {
	if rdb == nil || len(candidates) == 0 {
		return nil
	}

	keys := make([]string, 0, len(candidates))
	for _, c := range candidates {
		keys = append(keys, gateDrainingKey(c.NodeID))
	}

	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		logx.Errorf("[GateDrain] query draining gates failed, treating all as available: %v", err)
		return nil
	}

	draining := make(map[uint32]bool, len(candidates))
	for i, v := range values {
		if i < len(candidates) && v != nil {
			draining[candidates[i].NodeID] = true
		}
	}
	return draining
}

// FilterDrainingGates 从候选集里剔除正在排空的 gate。
//
// **全部都在排空时返回原集合**,不返回空。
//
// 这一条是刻意的:那意味着运维把整个 zone 的 gate 都标了排空。此时拒绝所有
// 登录,比把玩家分到一台待缩容的 gate 更糟 —— 后者只是稍后会被改派,前者是
// 直接进不去游戏。同时打 ERROR 让这个明显的误操作可见。
func FilterDrainingGates(candidates []GateCandidate, draining map[uint32]bool) []GateCandidate {
	if len(draining) == 0 || len(candidates) == 0 {
		return candidates
	}

	kept := make([]GateCandidate, 0, len(candidates))
	for _, c := range candidates {
		if !draining[c.NodeID] {
			kept = append(kept, c)
		}
	}

	if len(kept) == 0 {
		logx.Errorf("[GateDrain] every gate candidate (%d) is marked draining; ignoring the "+
			"marks so players can still log in — check whether a scale-in marked the whole "+
			"zone by mistake", len(candidates))
		return candidates
	}
	return kept
}
