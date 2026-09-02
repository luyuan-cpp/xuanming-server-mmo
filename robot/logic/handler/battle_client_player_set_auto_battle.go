package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerSetAutoBattleHandler 处理 SetAutoBattle 应答:
// 服务端返回错误提示时打 Warn,否则打一条确认日志。
func BattleClientPlayerSetAutoBattleHandler(player *gameobject.Player, response *battle.SetAutoBattleResponse) {
	if response == nil {
		zap.L().Warn("nil SetAutoBattleResponse", zap.Uint64("player", player.ID))
		return
	}
	if tip := response.GetErrorMessage(); tip != nil {
		zap.L().Warn("set auto battle rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("tip_id", tip.GetId()),
			zap.Strings("tip_params", tip.GetParameters()),
		)
		return
	}
	zap.L().Info("set auto battle ok", zap.Uint64("player", player.ID))
}
