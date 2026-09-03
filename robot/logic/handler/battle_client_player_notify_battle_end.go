package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifyBattleEndHandler 处理战斗结束推送:
// 记录结果并唤醒等待终局的脚本化场景(battle-smoke 等)。
func BattleClientPlayerNotifyBattleEndHandler(player *gameobject.Player, response *battle.BattleEndS2C) {
	if response == nil {
		zap.L().Warn("nil BattleEndS2C", zap.Uint64("player", player.ID))
		return
	}
	// 带 battle_id:登录时补推的上一局(离线挂起结算)BattleEnd 必须被过滤掉,见 Player.SignalBattleEnd。
	player.SignalBattleEnd(response.GetBattleId(), int32(response.GetOutcome()))
	zap.L().Info("notify battle end",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.String("outcome", response.GetOutcome().String()),
	)
}
