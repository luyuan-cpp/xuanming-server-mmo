package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifySpectateEndHandler 处理观战结束推送:
// 记录结束原因并唤醒等待观战终局的脚本化场景(battle-smoke 等)。
func BattleClientPlayerNotifySpectateEndHandler(player *gameobject.Player, response *battle.SpectateEndS2C) {
	if response == nil {
		zap.L().Warn("nil SpectateEndS2C", zap.Uint64("player", player.ID))
		return
	}
	player.SignalSpectateEnd(int32(response.GetReason()))
	zap.L().Info("notify spectate end",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.String("reason", response.GetReason().String()),
		zap.String("outcome", response.GetOutcome().String()),
	)
}
