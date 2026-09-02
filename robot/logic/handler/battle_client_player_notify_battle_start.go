package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifyBattleStartHandler 处理战斗开局推送:
// 记录 battleId 并唤醒等待开战的脚本化场景(battle-smoke 等)。
func BattleClientPlayerNotifyBattleStartHandler(player *gameobject.Player, response *battle.BattleStartS2C) {
	if response == nil {
		zap.L().Warn("nil BattleStartS2C", zap.Uint64("player", player.ID))
		return
	}
	player.SignalBattleStart(response.GetBattleId())
	zap.L().Info("notify battle start",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.Uint32("round_index", response.GetState().GetRoundIndex()),
		zap.Int("actors", len(response.GetState().GetActors())),
	)
}
