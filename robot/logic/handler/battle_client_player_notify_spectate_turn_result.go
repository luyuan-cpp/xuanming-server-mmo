package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifySpectateTurnResultHandler 处理观战侧回合结算推送:
// 观战回合计数 +1,供 battle-smoke 场景断言"观众确实看到了回合"。
func BattleClientPlayerNotifySpectateTurnResultHandler(player *gameobject.Player, response *battle.TurnResultS2C) {
	if response == nil {
		zap.L().Warn("nil spectate TurnResultS2C", zap.Uint64("player", player.ID))
		return
	}
	player.AddSpectateTurnResult()
	zap.L().Info("notify spectate turn result",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.Uint32("round_index", response.GetRoundIndex()),
		zap.Int("events", len(response.GetEvents())),
	)
}
