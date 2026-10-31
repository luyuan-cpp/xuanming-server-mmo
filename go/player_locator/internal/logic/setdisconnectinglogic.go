package logic

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

// staleGateInstanceTotal 计「请求与会话记录的 gate 实例号都非空且不等、按旧实例迟到的上报忽略」的次数
// (docs/design/scene-enter-chain-residuals.md §6.2)。op ∈ set_disconnecting | mark_offline,低基数,
// 不放 player_id;定位具体玩家靠同一处带两边实例号的 Info 日志。
// 稳态应接近 0;有数说明出现了同号跨实例(gate 以同一 node_id 重启 / 冲突收尾 / 跨 zone 同号),
// 被忽略的会话改由 session_reconciler 在旧实例从 etcd 消失后兜底清理(§6.8)。
// player_locator 此前没有自己的业务指标,这里沿用 login 的做法(go-zero core/metric,经 go-zero 自带的
// /metrics agent 暴露,开关见 etc/player_locator.yaml 的 Prometheus 段);markofflinelogic.go 共用这一个计数器。
var staleGateInstanceTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "player_locator",
	Subsystem: "",
	Name:      "stale_gate_instance_total",
	Help: "Disconnect / logout reports ignored because the request's gate instance differs from the session record's " +
		"(both non-empty). Op: set_disconnecting | mark_offline.",
	Labels: []string{"op"},
})

// staleGateInstanceTotal 的 op 取值。
const (
	staleGateInstanceOpSetDisconnecting = "set_disconnecting"
	staleGateInstanceOpMarkOffline      = "mark_offline"
)

// isStaleGateInstanceReport 判断一条断线 / 登出上报是否来自会话记录所指之外的 gate 实例:两边都非空且不等。
// "" 表示未知(旧版 login、早期会话记录),一律不比较,不当成「不等」(§1.4)。
// 记录里的实例号由已提交的 SetSession / Reconnect 写入,与 gate 上报所带的 SessionDetails.gate_instance_id 同源
// (node_uuid,进程启动时生成一次),所以被判陈旧的只会是跨实例的上报。
func isStaleGateInstanceReport(recordInstanceID, requestInstanceID string) bool {
	return recordInstanceID != "" && requestInstanceID != "" && recordInstanceID != requestInstanceID
}

type SetDisconnectingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetDisconnectingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetDisconnectingLogic {
	return &SetDisconnectingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *SetDisconnectingLogic) SetDisconnecting(in *pb.SetDisconnectingRequest) (*common.Empty, error) {
	key := sessionKey(in.PlayerId)

	// Get current session
	data, err := l.svcCtx.PlayerRedis.Get(l.ctx, key).Bytes()
	if err == redis.Nil {
		return &common.Empty{}, nil // No session, nothing to disconnect
	}
	if err != nil {
		return nil, err
	}

	session := &pb.PlayerSession{}
	if err := proto.Unmarshal(data, session); err != nil {
		return nil, err
	}

	// Only the current session can start disconnect
	if session.SessionId != in.SessionId {
		l.Infof("SetDisconnecting: session mismatch player=%d current=%d requested=%d, ignoring",
			in.PlayerId, session.SessionId, in.SessionId)
		return &common.Empty{}, nil
	}

	// 同号跨 gate 实例:旧实例迟到的断线上报不得给新实例上的会话挂租约,否则租约到期后活玩家被踢、
	// location 被删(docs/design/scene-enter-chain-residuals.md §6.1)。判定在 GET 到的同一份字节上做,
	// 随后的整字节 CAS 不变,比对与提交之间没有窗口。
	if isStaleGateInstanceReport(session.GateInstanceId, in.GateInstanceId) {
		staleGateInstanceTotal.Inc(staleGateInstanceOpSetDisconnecting)
		l.Infof("SetDisconnecting: stale gate instance player=%d session=%d record_instance=%s request_instance=%s, ignoring",
			in.PlayerId, in.SessionId, session.GateInstanceId, in.GateInstanceId)
		return &common.Empty{}, nil
	}

	// Calculate TTL
	ttl := time.Duration(in.LeaseTtlSeconds) * time.Second
	if ttl == 0 {
		ttl = time.Duration(l.svcCtx.Config.Lease.DefaultTTLSeconds) * time.Second
	}
	if ttl == 0 {
		ttl = 30 * time.Second
	}

	if session.State == pb.PlayerSessionState_SESSION_STATE_DISCONNECTING {
		// 同一断线事件的 RPC 重放不延长原租约，保持第一次调用的清理 deadline。
		return &common.Empty{}, nil
	}
	if session.State != pb.PlayerSessionState_SESSION_STATE_ONLINE {
		l.Infof("SetDisconnecting: player=%d state=%v, ignoring", in.PlayerId, session.State)
		return &common.Empty{}, nil
	}

	// 原地改写之前留一份旧会话(ONLINE),只用来推导要摘除的 gate 索引成员。
	previous := proto.Clone(session).(*pb.PlayerSession)

	// 状态变化也是 session 的真实变化，必须推进版本；monitor 的完整 protobuf
	// CAS 会因此同时约束 session_id 与 session_version。
	session.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	session.SessionVersion++
	updated, err := proto.Marshal(session)
	if err != nil {
		return nil, err
	}

	// 会话载荷不再靠 Redis TTL 删除，清理时机只由可靠的 lease ZSET 决定。
	//
	// 旧实现让会话 TTL 与 lease deadline 相等,于是 monitor claim 该玩家时,承载
	// GateId/GateInstanceId/SceneId/SceneNodeId 的会话键往往已被 Redis 删掉:
	// tick 相位与批量积压都保证 monitor 晚于 TTL。静态宽限也无法覆盖大规模断线。
	// 现在 ready -> processing claim 会保留载荷，外部副作用全部成功后才 ack；
	// 因此 session 必须活到显式 CAS 清理，不能再有独立的提前过期时钟。
	// 与会话对账共用同一个提交点(transitionToDisconnecting),gate 索引成员在同一段 Lua 里摘除。
	swapped, err := transitionToDisconnecting(l.ctx, l.svcCtx, in.PlayerId, data, updated, time.Now().Add(ttl).Unix(), previous)
	if err != nil {
		return nil, err
	}
	if !swapped {
		l.Infof("SetDisconnecting: session changed during CAS player=%d session=%d, ignoring stale disconnect",
			in.PlayerId, in.SessionId)
		return &common.Empty{}, nil
	}

	l.Infof("SetDisconnecting: player=%d session=%d version=%d ttl=%v",
		in.PlayerId, in.SessionId, session.SessionVersion, ttl)
	return &common.Empty{}, nil
}
