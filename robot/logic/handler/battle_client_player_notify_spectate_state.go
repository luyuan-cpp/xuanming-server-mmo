package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifySpectateStateHandler 处理观战全量快照推送:
// 记录观战对局 id / 观战人数,并唤醒等待观战就绪的脚本化场景(battle-smoke 等)。
func BattleClientPlayerNotifySpectateStateHandler(player *gameobject.Player, response *battle.SpectateStateS2C) {
	if response == nil {
		zap.L().Warn("nil SpectateStateS2C", zap.Uint64("player", player.ID))
		return
	}
	player.SignalSpectateState(response.GetState().GetBattleId(), response.GetObserverCount())
	zap.L().Info("notify spectate state",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetState().GetBattleId()),
		zap.Uint32("round_index", response.GetState().GetRoundIndex()),
		zap.Uint32("observer_count", response.GetObserverCount()),
	)
}
