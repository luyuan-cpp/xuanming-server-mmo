package logic

import (
	"context"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
)

type GetSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSessionLogic {
	return &GetSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *GetSessionLogic) GetSession(in *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	key := sessionKey(in.PlayerId)
	data, err := l.svcCtx.PlayerRedis.Get(l.ctx, key).Bytes()
	if err == redis.Nil {
		return &pb.GetSessionResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}

	session := &pb.PlayerSession{}
	if err := proto.Unmarshal(data, session); err != nil {
		return nil, err
	}

	return &pb.GetSessionResponse{Session: session, Found: true}, nil
}
