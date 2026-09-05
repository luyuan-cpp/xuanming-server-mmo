package logic

import (
	"fmt"

	"match/internal/svc"

	smpb "proto/scene_manager"

	"google.golang.org/protobuf/proto"
)

// loadPlayerLocation 读 player:{id}:location(scene_manager 写的 protobuf
// PlayerLocation,跨运行时契约 key,只经 SharedRedis 读)。键不存在返回 (nil, nil):
// 玩家不在线或未进场。JoinQueue 取 zone、gather 定位 scene 节点、观战取观众 zone
// 三处共用一份解析。
func loadPlayerLocation(svcCtx *svc.ServiceContext, playerId uint64) (*smpb.PlayerLocation, error) {
	raw, err := svcCtx.SharedRedis.Get(getPlayerLocationKey(playerId))
	if err != nil {
		return nil, fmt.Errorf("读玩家位置失败: %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), loc); err != nil {
		return nil, fmt.Errorf("玩家位置反序列化失败: %w", err)
	}
	return loc, nil
}
