package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifyTurnResultHandler 处理参战侧回合结算推送:
// 参战回合计数 +1,供 battle-smoke 场景断言"确实打了回合"。
func BattleClientPlayerNotifyTurnResultHandler(player *gameobject.Player, response *battle.TurnResultS2C) {
	if response == nil {
		zap.L().Warn("nil TurnResultS2C", zap.Uint64("player", player.ID))
		return
	}
	player.AddTurnResult()
	zap.L().Info("notify turn result",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.Uint32("round_index", response.GetRoundIndex()),
		zap.Int("events", len(response.GetEvents())),
	)
}
