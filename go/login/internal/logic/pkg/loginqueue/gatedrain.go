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
//  2. 等在线掉到阈值以下,或等到超时(gatedrain_monitor.go 判定并写 drained)。开了 GateDrain.Migrate 时,
//     这段时间里 gate 按限速把已登录的连接重定向到同 zone 的其他 gate(gatedrain_migrate.go,
//     docs/design/gate-drain-session-migration.md);没开时等玩家自然下线。
//  3. 看到 drained 才删 Pod;到期还剩的人随 Pod 下线断开,重连时落到别的 gate。
//
// 本文件只实现第 1 步与状态读写。删 Pod 由运维流程(tools/scripts/k8s_gate_drain.ps1)驱动 —— 刻意不做成
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
// 体现在登录延迟上(压测口径见 CLAUDE.md §6)。选 gate 的热路径不直接调它,经 DrainMarkCache
// 按 zone 缓存一次结果(2026-11-05,缺口清单第 31 条),同一 zone 每个刷新周期只查一次。
//
// 查询失败时返回 nil(视作"没人在排空")而不是报错:见 MarkDrainingGates
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

// MarkDrainingGates 就地给候选打排空标记(GateCandidate.Draining),不剔除。
//
// 排空中的 gate 留在候选集里有两个原因(2026-11-06 起;此前这里直接剔除):
//   - 容量记账(FreeSlots)要数到它上面还在线的人。剔掉之后区级准入少算在线,排空期间会多放人进来;
//   - 挑哪台由 eligibleGates 一次分档决定(规则在 shared/gatepick.Eligible):有完全合格的 gate 时不会挑到它;
//     没有时它排在停写的 gate 前面 —— 它连得上,停写的多半连不上。整个 zone 都标了排空时照样能登录,
//     比拒绝所有登录好(这多半是误操作,玩家稍后随排空流程改派),回落计入 login_gate_pick_fallback_total{reason="draining"}。
//
// draining 为空时什么都不做;查询失败时 DrainingGates 返回 nil,等于没人在排空(放行)。
// candidates 必须是调用方自己的切片(buildGateCandidates 每次新建),本函数会改它的元素。
func MarkDrainingGates(candidates []GateCandidate, draining map[uint32]bool) {
	if len(draining) == 0 {
		return
	}
	for i := range candidates {
		if draining[candidates[i].NodeID] {
			candidates[i].Draining = true
		}
	}
}
