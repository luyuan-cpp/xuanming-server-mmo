package svc

import (
	"context"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/node"
	login_proto "proto/common/base"
	kafkapb "proto/contracts/kafka"
	loginpb "proto/login"
)

// gate 排空迁移在 login 这一侧的 I/O(docs/design/gate-drain-session-migration.md §4.4):
//   - 排空判定每一拍给排空中的 gate 发迁移许可(sendDrainMigrationLeases,由 startGateDrainMonitor 调);
//   - 为 gate 发来的 RequestGateMigration 选目标、签票(SignGateMigration)。
//
// 判定都在纯函数里:loginqueue.DecideDrainMigration、loginqueue.PickMigrationTarget 与下面的 planGateMigration。

// gateMigrationTokenTTL 与 scene_manager 跨 zone 重定向的票据同长(gate_redirect.go 的 redirectTokenTTLSeconds)。
// 票据推下去之后客户端几秒内就会用,5 分钟只是给慢网络和 RedirectFlow 的探测留的余量。
const gateMigrationTokenTTL = 5 * time.Minute

// 两个计数器的 result 标签都是封闭集合,不带 player_id 或节点号(AGENTS.md §9)。
var (
	gateMigrationSignTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_migration",
		Name:      "sign_total",
		Help: "RequestGateMigration results: ok, disabled, not_draining (caller is not a draining gate of this zone), " +
			"no_target (no eligible gate to move to; no fallback), not_logged_in (an unbound ticket was asked for a " +
			"connection without a login session), no_identity, no_secret, failed (directory / Redis / signing error).",
		Labels: []string{"result"},
	})
	gateMigrationLeaseTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "login",
		Subsystem: "gate_migration",
		Name:      "lease_total",
		Help: "Migration leases the gate drain monitor sent to draining gates: sent, send_failed. Every login replica " +
			"sends one per draining gate per monitor tick; duplicates are expected.",
		Labels: []string{"result"},
	})
)

// gateDrainMigratePolicy 把配置换成发许可的参数。
//   - 租期取 3 × 判定周期:单个 login 副本漏发一两拍不会让迁移断掉,排空取消或开关关掉之后最多这么久 gate 停下;
//   - StartAfter 向上取整到秒,<= 0 一律 0(立即开始),与 gateDrainPolicy 对 Deadline 的取整同一口径。
func gateDrainMigratePolicy(cfg config.GateDrainConf) loginqueue.DrainMigratePolicy {
	var leaseMs uint32
	if lease := 3 * cfg.Interval; lease > 0 {
		ms := lease.Milliseconds()
		if ms > math.MaxUint32 {
			ms = math.MaxUint32
		}
		leaseMs = uint32(ms)
	}
	var startAfterSeconds int64
	if cfg.Migrate.StartAfter > 0 {
		startAfterSeconds = int64((cfg.Migrate.StartAfter + time.Second - 1) / time.Second)
	}
	return loginqueue.DrainMigratePolicy{
		Enabled:             cfg.Migrate.Enabled,
		RatePerSecond:       cfg.Migrate.RatePerSecond,
		StartAfterSeconds:   startAfterSeconds,
		DrainedBelowPlayers: cfg.DrainedBelowPlayers,
		LeaseMs:             leaseMs,
	}
}

// sendDrainMigrationLeases 给这一拍该收许可的 gate 各发一条 GateDrainMigrationEvent。
// gates 是排空判定同一拍的快照(直读 etcd,带实例 uuid)。发送失败只计数、打日志:下一拍会重发,
// gate 在租期内照常迁移。
func (s *ServiceContext) sendDrainMigrationLeases(ctx context.Context, gates []loginqueue.GateOnline, p loginqueue.DrainMigratePolicy) {
	for _, lease := range loginqueue.DrainMigrationsDue(ctx, s.ControlRedis, config.AppConfig.Node.ZoneId, gates, p, time.Now().Unix()) {
		gateID := strconv.FormatUint(uint64(lease.NodeID), 10)
		err := s.SendGateDrainMigration(gateID, lease.InstanceID, &kafkapb.GateDrainMigrationEvent{
			DrainMarkedAt: lease.DrainMarkedAt,
			RatePerSecond: lease.RatePerSecond,
			LeaseMs:       lease.LeaseMs,
		})
		if err != nil {
			gateMigrationLeaseTotal.Inc("send_failed")
			logx.Errorf("[GateDrain] gate %s: send migration lease failed, retry next tick: %v", gateID, err)
			continue
		}
		gateMigrationLeaseTotal.Inc("sent")
		logx.Infof("[GateDrain] gate %s: migration lease sent (online=%d rate=%d/s lease=%dms drain_marked_at=%d)",
			gateID, lease.Online, lease.RatePerSecond, lease.LeaseMs, lease.DrainMarkedAt)
	}
}

// SendGateDrainMigration 经 Kafka 给一台 gate 发迁移许可。
//
// 与 Bind / Kick / PushTip 同一个生产者、同一条寻址收口(buildGateCommandMessage 的三道 fail-closed 守卫照样生效)。
func (s *ServiceContext) SendGateDrainMigration(gateID string, gateInstanceID string, event *kafkapb.GateDrainMigrationEvent) error {
	msg, err := buildGateDrainMigrationCommand(gateID, gateInstanceID, event)
	if err != nil {
		return err
	}
	if err := s.KafkaClient.SendToTopicPartition(msg.Topic, msg.Partition, msg.Payload, msg.PartitionKey); err != nil {
		return fmt.Errorf("send gate drain migration command to %s/%d: %w", msg.Topic, msg.Partition, err)
	}
	return nil
}

// SignGateMigration 为 gate 发来的 RequestGateMigration 选目标、签票。details 是经 callerauth 验过签的会话身份。
//
// 不写 Redis、不改会话:同一个连接重复请求只会多签一张没人用的票。结果按封闭集合计数。
func (s *ServiceContext) SignGateMigration(ctx context.Context, details *login_proto.SessionDetails) *loginpb.GateMigrationResponse {
	resp, outcome := s.signGateMigration(ctx, details)
	gateMigrationSignTotal.Inc(outcome)
	return resp
}

func migrationResult(r loginpb.GateMigrationResult) *loginpb.GateMigrationResponse {
	return &loginpb.GateMigrationResponse{Result: r}
}

func (s *ServiceContext) signGateMigration(ctx context.Context, details *login_proto.SessionDetails) (*loginpb.GateMigrationResponse, string) {
	unspecified := migrationResult(loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_UNSPECIFIED)
	if !config.AppConfig.GateDrain.Migrate.Enabled {
		return migrationResult(loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_DISABLED), "disabled"
	}
	secret := s.GateTokenSigningSecret()
	if len(secret) == 0 {
		logx.Errorf("[GateMigration] gate token signing secret is not configured; cannot sign migration tickets")
		return unspecified, "no_secret"
	}
	ref, ok := loginsession.RefOf(details)
	if !ok || details.GetGateNodeId() == 0 {
		logx.Errorf("[GateMigration] rejected: incomplete gate connection identity gate_node_id=%d gate_instance=%q incarnation=%d",
			details.GetGateNodeId(), details.GetGateInstanceId(), details.GetSessionIncarnation())
		return unspecified, "no_identity"
	}
	if s.GateWatcher == nil || s.ControlRedis == nil {
		return unspecified, "failed"
	}

	// 选角界面的连接(还没绑角色)签的是不绑持票人的票,等同一张普通 AssignGate 票据。先确认这条连接确实登录过:
	// 迁移是给已登录的连接换个 gate,不能变成一条绕过排队拿票的路。游戏中的连接票据绑定本人,EnterGame 还要先 Login。
	if details.GetPlayerId() == 0 {
		if _, err := loginsession.GetAccount(ctx, s.LoginRedis, ref); err != nil {
			if errors.Is(err, redis.Nil) {
				return unspecified, "not_logged_in"
			}
			logx.Errorf("[GateMigration] read login session failed: gate_node_id=%d err=%v", details.GetGateNodeId(), err)
			return unspecified, "failed"
		}
	}

	nodes, err := s.GateWatcher.Nodes(ctx)
	if err != nil {
		logx.Errorf("[GateMigration] read gate directory failed: %v", err)
		return unspecified, "failed"
	}
	zoneID := config.AppConfig.Node.ZoneId
	// 读失败就不签(fail-closed,见 DrainingGateSet):不知道谁在排空时,不能把人迁过去。
	draining, err := loginqueue.DrainingGateSet(ctx, s.ControlRedis, zoneID, zoneGateIDs(nodes, zoneID))
	if err != nil {
		logx.Errorf("[GateMigration] %v", err)
		return unspecified, "failed"
	}

	target, result := planGateMigration(nodes, zoneID, config.AppConfig.RequireClientEndpoint,
		details.GetGateNodeId(), details.GetGateInstanceId(), draining, mathrand.IntN)
	switch result {
	case loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_OK:
	case loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NOT_DRAINING:
		return migrationResult(result), "not_draining"
	case loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NO_TARGET:
		return migrationResult(result), "no_target"
	default:
		return unspecified, "failed"
	}

	admit, err := loginqueue.SignMigrationGateToken(target, secret, gateMigrationTokenTTL, details.GetPlayerId())
	if err != nil {
		logx.Errorf("[GateMigration] sign ticket for gate %d failed: %v", target.NodeID, err)
		return unspecified, "failed"
	}
	return &loginpb.GateMigrationResponse{
		Result:         loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_OK,
		TargetGateIp:   admit.IP,
		TargetGatePort: admit.Port,
		TokenPayload:   admit.TokenPayload,
		TokenSignature: admit.TokenSignature,
		TokenDeadline:  admit.TokenDeadline,
	}, "ok"
}

// planGateMigration 是签票之前的全部判定,纯函数。nodes 是 gate 目录(watch 镜像),draining 是本 zone 的排空标记。
//
//  1. 调用方必须是本 zone 目录里的这一台:node_id 与实例 uuid 都对上。路由服把请求送错 zone,或旧实例迟到的请求,
//     都在这里挡下。否则票据会把人送进别的 zone,或者替一台已经不在的 gate 迁人。
//  2. 调用方必须正在排空:gate 的 bug 或迟到的许可,不能把一台健康 gate 上的人迁走。
//  3. 选目标:候选与登录挑 gate 用同一份 buildGateCandidates(按 zone 过滤、选客户端地址、按地址去重),
//     再由 PickMigrationTarget 剔除调用方、排空、停写与满载,不回落。
//
// 第 1、2 条不过回 NOT_DRAINING(gate 停止本轮迁移),第 3 条选不出回 NO_TARGET(这个连接稍后再试)。
func planGateMigration(nodes []node.WatchedNode, zoneID uint32, requireClientEndpoint bool,
	callerNodeID uint32, callerInstanceID string, draining map[uint32]bool, intn func(int) int,
) (*loginqueue.GateCandidate, loginpb.GateMigrationResult) {
	if !gateRegisteredInZone(nodes, zoneID, callerNodeID, callerInstanceID) || !draining[callerNodeID] {
		return nil, loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NOT_DRAINING
	}
	target, ok := loginqueue.PickMigrationTarget(buildGateCandidates(nodes, zoneID, requireClientEndpoint), draining, callerNodeID, intn)
	if !ok {
		return nil, loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NO_TARGET
	}
	return target, loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_OK
}

// gateRegisteredInZone 报告目录里有没有这台 gate:同 zone、同 node_id、同实例 uuid。实例 uuid 为空一律不算。
func gateRegisteredInZone(nodes []node.WatchedNode, zoneID uint32, nodeID uint32, instanceID string) bool {
	if nodeID == 0 || instanceID == "" {
		return false
	}
	for _, w := range nodes {
		n := w.Info
		if n != nil && n.GetZoneId() == zoneID && n.GetNodeId() == nodeID && n.GetNodeUuid() == instanceID {
			return true
		}
	}
	return false
}

// zoneGateIDs 返回目录里本 zone 的 gate node_id,去重、保持出现顺序。排空标记按 node_id 打,一次 MGET 查完。
func zoneGateIDs(nodes []node.WatchedNode, zoneID uint32) []uint32 {
	seen := make(map[uint32]bool, len(nodes))
	ids := make([]uint32, 0, len(nodes))
	for _, w := range nodes {
		n := w.Info
		if n == nil || n.GetZoneId() != zoneID || seen[n.GetNodeId()] {
			continue
		}
		seen[n.GetNodeId()] = true
		ids = append(ids, n.GetNodeId())
	}
	return ids
}
