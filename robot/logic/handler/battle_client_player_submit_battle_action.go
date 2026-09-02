package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerSubmitBattleActionHandler 处理 SubmitBattleAction 应答:
// 服务端返回错误提示时打 Warn(动作被拒),否则静默(成功应答量大,避免刷屏)。
func BattleClientPlayerSubmitBattleActionHandler(player *gameobject.Player, response *battle.SubmitBattleActionResponse) {
	if response == nil {
		zap.L().Warn("nil SubmitBattleActionResponse", zap.Uint64("player", player.ID))
		return
	}
	if tip := response.GetErrorMessage(); tip != nil {
		zap.L().Warn("submit battle action rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("tip_id", tip.GetId()),
			zap.Strings("tip_params", tip.GetParameters()),
		)
	}
}
