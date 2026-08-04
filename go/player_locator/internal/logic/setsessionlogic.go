package logic

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

type SetSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetSessionLogic {
	return &SetSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *SetSessionLogic) SetSession(in *pb.SetSessionRequest) (*common.Empty, error) {
	incoming := in.GetSession()
	if incoming == nil {
		return nil, fmt.Errorf("session is required")
	}
	if incoming.PlayerId == 0 {
		return nil, fmt.Errorf("session player_id is required")
	}

	// clone 后再补继承字段，避免修改 gRPC request 自身。
	s := proto.Clone(incoming).(*pb.PlayerSession)
	key := sessionKey(s.PlayerId)
	expected, err := l.svcCtx.RedisClient.Get(l.ctx, key).Bytes()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	if err == redis.Nil {
		expected = nil
	} else {
		current := &pb.PlayerSession{}
		if err := proto.Unmarshal(expected, current); err != nil {
			return nil, fmt.Errorf("decode current session: %w", err)
		}

		// ReplaceLogin 的调用方当前不会把旧 SceneID/SceneNodeID 拷进新会话。
		// 在现有 RPC 契约下保留旧值，至少避免顶号登录把已知场景位置清成 0。
		if s.SceneId == 0 {
			s.SceneId = current.SceneId
		}
		if s.SceneNodeId == "" {
			s.SceneNodeId = current.SceneNodeId
		}

		if proto.Equal(s, current) {
			// RPC 响应丢失后的同请求重放：允许幂等地清掉残留 lease 状态。
		} else if s.SessionVersion != current.SessionVersion+1 {
			return nil, fmt.Errorf(
				"stale session update for player %d: current version=%d incoming=%d",
				s.PlayerId, current.SessionVersion, s.SessionVersion,
			)
		}
	}

	data, err := proto.Marshal(s)
	if err != nil {
		return nil, err
	}

	// session 写入与旧 ready/processing lease 的撤销在同一 Lua 中完成。
	// 完整 protobuf CAS 防止两个基于同一旧版本构造的替换登录互相覆盖。
	result, err := replaceSessionAndCancelLease(l.ctx, l.svcCtx, s.PlayerId, expected, data, 0)
	if err != nil {
		return nil, err
	}
	if result == replaceSessionCleanupPending {
		return nil, fmt.Errorf("%w for player %d", ErrSessionCleanupPending, s.PlayerId)
	}
	if result != replaceSessionApplied {
		return nil, fmt.Errorf("session changed concurrently for player %d", s.PlayerId)
	}

	return &common.Empty{}, nil
}
