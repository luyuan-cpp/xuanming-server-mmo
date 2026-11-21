package logic

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
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
	if !in.GetIncludePlacement() {
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

	// 同槽位一次 MGET 原子读取 session 与 scene_manager 权威 location，不改写 PlayerSession 原有字段。
	values, err := l.svcCtx.PlayerRedis.MGet(l.ctx, key, sceneManagerLocationKey(in.PlayerId)).Result()
	if err != nil {
		return nil, err
	}
	if len(values) != 2 {
		return nil, fmt.Errorf("unexpected mget reply length %d for player %d", len(values), in.PlayerId)
	}

	resp := &pb.GetSessionResponse{
		Placement: &pb.PlayerScenePlacement{Found: false},
	}
	if rawSession, ok := mgetBytes(values[0]); ok {
		session := &pb.PlayerSession{}
		if err := proto.Unmarshal(rawSession, session); err != nil {
			return nil, err
		}
		resp.Session = session
		resp.Found = true
	}
	if rawLoc, ok := mgetBytes(values[1]); ok && len(rawLoc) > 0 {
		loc := &smpb.PlayerLocation{}
		if err := proto.Unmarshal(rawLoc, loc); err != nil {
			return nil, fmt.Errorf("decode SceneManager location for player %d: %w", in.PlayerId, err)
		}
		resp.Placement = &pb.PlayerScenePlacement{
			Found:             true,
			ZoneId:            loc.GetZoneId(),
			NodeId:            loc.GetNodeId(),
			SceneId:           loc.GetSceneId(),
			OwnerEpoch:        loc.GetOwnerEpoch(),
			AwaitingPlacement: loc.GetNodeId() == "" && loc.GetOwnerEpoch() != 0,
			SessionId:         loc.GetSessionId(),
			LoginGeneration:   loc.GetLoginGeneration(),
		}
	}
	return resp, nil
}

func mgetBytes(value any) ([]byte, bool) {
	switch raw := value.(type) {
	case string:
		return []byte(raw), true
	case []byte:
		return raw, true
	default:
		return nil, false
	}
}
