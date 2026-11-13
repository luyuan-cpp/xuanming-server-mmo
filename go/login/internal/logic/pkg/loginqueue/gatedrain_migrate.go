package loginqueue

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/gatepick"
)

// gate 排空时把在线连接主动迁到同 zone 的其他 gate(docs/design/gate-drain-session-migration.md)。
//
// 本文件只放 login 这一侧的判定,不碰会话、不写 Redis:
//   - DecideDrainMigration / DrainMigrationsDue:排空判定每一拍,哪些 gate 该收到迁移许可;
//   - PickMigrationTarget:为一个要迁走的连接选目标 gate,不回落;
//   - DrainingGateSet:fail-closed 地读排空标记(签票时用)。
//
// 发许可与签票的 I/O 在 login/internal/svc/gate_migration.go。gatedrain_monitor.go 文件头「不主动处理」的两条理由
// 对踢线成立、对重定向不成立:客户端收到 msg 124 会自己换连接并重登,不弹「账号在别处登录」,见设计文档 §1。

// DrainMigratePolicy 是发迁移许可的参数(由 config.GateDrainMigrateConf 与 GateDrain.Interval 换算)。
type DrainMigratePolicy struct {
	Enabled bool
	// RatePerSecond 原样写进许可,gate 每秒至多发起这么多个迁移请求;0 等同关闭。
	RatePerSecond uint32
	// StartAfterSeconds:标记 draining 之后满这么久才发许可。
	StartAfterSeconds int64
	// DrainedBelowPlayers 与排空判定同值:在线数已不大于它的 gate 不再需要迁移。
	DrainedBelowPlayers uint32
	// LeaseMs 原样写进许可:gate 收到后这么久没收到下一条就停。取 3 × 判定周期,单个 login 副本漏发一两拍不会让迁移断掉。
	LeaseMs uint32
}

func (p DrainMigratePolicy) active() bool {
	return p.Enabled && p.RatePerSecond > 0 && p.LeaseMs > 0
}

// DrainMigrationLease 是这一拍要发给一台排空中 gate 的迁移许可,字段一一对应 GateDrainMigrationEvent 与它的寻址。
type DrainMigrationLease struct {
	NodeID        uint32
	InstanceID    string
	DrainMarkedAt int64
	Online        uint32
	RatePerSecond uint32
	LeaseMs       uint32
}

// DecideDrainMigration 判断这一拍要不要给 g 发迁移许可。纯函数,markedAtRaw 是这台 gate 排空标记(gatepick.DrainingKey)的原值
// (空串 = 不在排空)。
//
// 要发,必须同时满足:
//   - 迁移已开启,速率与租期都大于 0;
//   - g 正在排空;
//   - g 有实例 uuid:命令要定向到这个实例(AGENTS.md §7.2),没有就发不出去,由调用方计数;
//   - g 的在线数大于 DrainedBelowPlayers:已经够排空阈值的 gate 不必再迁;
//   - 距标记时刻已满 StartAfterSeconds。
//
// 标记值解析不了时按 0 处理,与 EvaluateDrainingGates 同口径(那边同样据此判期限)。时钟回拨或标记来自别的机器、
// 算出负数时按刚标记处理,宁可晚迁。
func DecideDrainMigration(g GateOnline, markedAtRaw string, nowUnix int64, p DrainMigratePolicy) (DrainMigrationLease, bool) {
	if !p.active() || markedAtRaw == "" || g.InstanceID == "" {
		return DrainMigrationLease{}, false
	}
	if g.PlayerCount <= p.DrainedBelowPlayers {
		return DrainMigrationLease{}, false
	}
	markedAt, _ := strconv.ParseInt(markedAtRaw, 10, 64)
	waited := nowUnix - markedAt
	if waited < 0 {
		waited = 0
	}
	if waited < p.StartAfterSeconds {
		return DrainMigrationLease{}, false
	}
	return DrainMigrationLease{
		NodeID:        g.NodeID,
		InstanceID:    g.InstanceID,
		DrainMarkedAt: markedAt,
		Online:        g.PlayerCount,
		RatePerSecond: p.RatePerSecond,
		LeaseMs:       p.LeaseMs,
	}, true
}

// DrainMigrationsDue 用一次 MGET 读快照里全部 gate 的排空标记,返回这一拍要发许可的 gate。gates 都是 zoneID 这个 zone 的
// gate(本 login 的 Node.ZoneId),它们的排空键同槽。
//
// 读失败时本拍一条都不发:已经在迁的 gate 在租期内照常迁,租期到了就停 —— 方向是少迁,不误迁。
// 快照里没有实例 uuid 的排空中 gate 也不发(DecideDrainMigration),这里打一条节流的 ERROR。
func DrainMigrationsDue(ctx context.Context, rdb redis.UniversalClient, zoneID uint32, gates []GateOnline, p DrainMigratePolicy, nowUnix int64) []DrainMigrationLease {
	if rdb == nil || len(gates) == 0 || !p.active() {
		return nil
	}
	keys := make([]string, len(gates))
	for i, g := range gates {
		keys[i] = gatepick.DrainingKey(zoneID, g.NodeID)
	}
	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		logx.Errorf("[GateDrain] read draining marks for migration failed, no migration lease this round: %v", err)
		return nil
	}
	var due []DrainMigrationLease
	for i, g := range gates {
		raw := ""
		if i < len(values) {
			raw, _ = values[i].(string)
		}
		if raw != "" && g.InstanceID == "" && missingInstanceLog.allow(gatePickLogEvery) {
			logx.Errorf("[GateDrain] gate %d is draining but its registration has no instance uuid; "+
				"cannot address a migration lease to it (further reports suppressed for %s)", g.NodeID, gatePickLogEvery)
		}
		if lease, ok := DecideDrainMigration(g, raw, nowUnix, p); ok {
			due = append(due, lease)
		}
	}
	return due
}

var missingInstanceLog logThrottle

// DrainingGateSet 读 zoneID 里 nodeIDs 这些 gate 哪些正在排空,读失败返回错误。
//
// 与 DrainingGates 的区别只在失败方向:登录挑 gate 时读不到标记要放行(那条路径拦下就是整区登不上),
// 迁移签票时读不到就不签 —— 迁移是可以等的计划内动作,宁可这一次不迁,也不在不知道谁在排空时把人迁过去。
func DrainingGateSet(ctx context.Context, rdb redis.UniversalClient, zoneID uint32, nodeIDs []uint32) (map[uint32]bool, error) {
	if rdb == nil {
		return nil, fmt.Errorf("gate drain: nil redis client")
	}
	if len(nodeIDs) == 0 {
		return map[uint32]bool{}, nil
	}
	keys := make([]string, len(nodeIDs))
	for i, id := range nodeIDs {
		keys[i] = gatepick.DrainingKey(zoneID, id)
	}
	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read gate draining marks: %w", err)
	}
	draining := make(map[uint32]bool, len(nodeIDs))
	for i, v := range values {
		if i < len(nodeIDs) && v != nil {
			draining[nodeIDs[i]] = true
		}
	}
	return draining, nil
}

// PickMigrationTarget 为一个要从 callerNodeID 迁走的连接选目标 gate。纯函数,intn 是随机源(同 PickGate)。
//
// 候选里剔除调用方自己、正在排空的、停写的与满载的,再随机二选一。**不回落**:
//   - 登录时「全部不合格就回落全集」是为了不把人挡在门外;
//   - 迁移时回落只会把人推到满载、卡住或也要下线的 gate 上。
//
// 合格的目标再分两档(设计文档 §7.3):有不在外出构建上的,就只在它们里面挑;一个都没有才用全部合格目标。
// 逐台换版时,被排空的永远是要换下的那个构建,所以这样挑出来的就是已经换上新构建的 gate,玩家迁过去之后不会再被迁一次。
//
// 没有合格的目标返回 false,这个连接留在原地,之后的拍子再试或等到期(设计文档 §4.4)。
// candidates 应已按 zone 过滤并按客户端地址去重(buildGateCandidates 的结果),不得被修改。
func PickMigrationTarget(candidates []GateCandidate, draining map[uint32]bool, callerNodeID uint32, intn func(int) int) (*GateCandidate, bool) {
	targets := make([]GateCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.NodeID == callerNodeID || draining[c.NodeID] {
			continue
		}
		if gatepick.Verdict(candidateState(c)) != "" {
			continue
		}
		targets = append(targets, c)
	}
	targets = preferIncomingBuild(targets, outgoingBuilds(candidates, draining))
	i, ok := gatepick.TwoChoices(targets, func(c GateCandidate) uint32 { return c.PlayerCount }, intn)
	if !ok {
		return nil, false
	}
	target := targets[i]
	return &target, true
}

// outgoingBuilds 返回外出构建:候选里至少有一台排空中 gate 的构建。没上报构建(空串)的不算,没有依据就不偏好。
//
// 与 scene_manager 的同名概念同一口径(docs/design/scene-node-planned-drain.md §4.6):不需要知道哪个版本更新,
// 升级时被排空的是旧版本,回退时被排空的是新版本,「避开外出构建」两种情况下都是「迁到要留下的那个版本」。
func outgoingBuilds(candidates []GateCandidate, draining map[uint32]bool) map[string]bool {
	var outgoing map[string]bool
	for _, c := range candidates {
		if c.Build == "" || !draining[c.NodeID] {
			continue
		}
		if outgoing == nil {
			outgoing = make(map[string]bool)
		}
		outgoing[c.Build] = true
	}
	return outgoing
}

// preferIncomingBuild 在合格目标里只留不在外出构建上的。一个都没有(第一台排空时还没有新构建的 gate,或新构建的都满了、
// 同构建重启)就原样返回,不因为偏好把人留在要下线的 gate 上。没上报构建的目标算首选。不修改入参。
func preferIncomingBuild(targets []GateCandidate, outgoing map[string]bool) []GateCandidate {
	if len(outgoing) == 0 {
		return targets
	}
	preferred := make([]GateCandidate, 0, len(targets))
	for _, c := range targets {
		if c.Build == "" || !outgoing[c.Build] {
			preferred = append(preferred, c)
		}
	}
	if len(preferred) == 0 {
		return targets
	}
	return preferred
}
