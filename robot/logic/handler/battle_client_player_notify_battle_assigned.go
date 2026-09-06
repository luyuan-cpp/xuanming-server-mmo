package handler

import (
	"go.uber.org/zap"

	"proto/battle"
	"robot/logic/gameobject"
)

// BattleClientPlayerNotifyBattleAssignedHandler 处理直连落点分配推送
// (docs/design/turn-based-battle-server.md §18 D26):记录 host:port + 票据并唤醒
// 等待直连的脚本化场景(battle-smoke 的 openBattleDirectConn)。
func BattleClientPlayerNotifyBattleAssignedHandler(player *gameobject.Player, response *battle.BattleAssignedS2C) {
	if response == nil {
		zap.L().Warn("nil BattleAssignedS2C", zap.Uint64("player", player.ID))
		return
	}
	player.SignalBattleAssigned(response)
	zap.L().Info("notify battle assigned",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
		zap.String("host", response.GetHost()),
		zap.Uint32("port", response.GetPort()),
		zap.String("role", response.GetRole().String()),
		zap.Uint64("expire_at_ms", response.GetExpireAtMs()),
		zap.Int("signature_len", len(response.GetTokenSignature())),
	)
}
