package logic

import (
	"context"

	"match/internal/playercontract"
	"match/internal/svc"

	smpb "proto/scene_manager"
)

// loadPlayerLocation 读 player:{id}:location(scene_manager 写的 protobuf
// PlayerLocation,跨运行时契约 key,只经 SharedRedis 读)。键不存在返回 (nil, nil):
// 玩家不在线或未进场。JoinQueue 取 zone、gather 定位 scene 节点、观战取观众 zone
// 三处共用一份解析。
//
// 实现收口在 playercontract.LoadLocation(team-system.md §A.2);这里传
// context.Background(),与改造前 go-zero 无 ctx 的 Get 行为一致,调用点不用改。
func loadPlayerLocation(svcCtx *svc.ServiceContext, playerId uint64) (*smpb.PlayerLocation, error) {
	return playercontract.LoadLocation(context.Background(), svcCtx, playerId)
}
