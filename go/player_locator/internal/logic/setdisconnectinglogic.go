package logic

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

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
	data, err := l.svcCtx.RedisClient.Get(l.ctx, key).Bytes()
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
	playerID := strconv.FormatUint(in.PlayerId, 10)
	swapped, err := setDisconnectingScript.Run(
		l.ctx,
		l.svcCtx.RedisClient,
		sessionLifecycleKeys(in.PlayerId),
		data,
		updated,
		0,
		time.Now().Add(ttl).Unix(),
		playerID,
	).Int()
	if err != nil {
		return nil, err
	}
	if swapped != 1 {
		l.Infof("SetDisconnecting: session changed during CAS player=%d session=%d, ignoring stale disconnect",
			in.PlayerId, in.SessionId)
		return &common.Empty{}, nil
	}

	l.Infof("SetDisconnecting: player=%d session=%d version=%d ttl=%v",
		in.PlayerId, in.SessionId, session.SessionVersion, ttl)
	return &common.Empty{}, nil
}
