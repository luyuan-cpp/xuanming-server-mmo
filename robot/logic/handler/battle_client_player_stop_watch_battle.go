package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerStopWatchBattleHandler 处理 StopWatchBattle 应答:
// 服务端返回错误提示时打 Warn,否则打一条确认日志。
func BattleClientPlayerStopWatchBattleHandler(player *gameobject.Player, response *battle.StopWatchBattleResponse) {
	if response == nil {
		zap.L().Warn("nil StopWatchBattleResponse", zap.Uint64("player", player.ID))
		return
	}
	if tip := response.GetErrorMessage(); tip != nil {
		zap.L().Warn("stop watch battle rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("tip_id", tip.GetId()),
			zap.Strings("tip_params", tip.GetParameters()),
		)
		return
	}
	zap.L().Info("stop watch battle ok", zap.Uint64("player", player.ID))
}
