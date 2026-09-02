package handler

// match_service 客户端应答 handler(手写,非生成器产物)。
// battle-smoke 场景发 JoinQueue / WatchBattle 后,服务端的应答以同 message id
// 回到客户端;这里只做诊断日志:错误提示打 Warn,成功打 Info(带 ticket / battle_id)。

import (
	"go.uber.org/zap"

	"proto/match"
	"robot/logic/gameobject"
)

// MatchServiceJoinQueueHandler 处理 JoinQueue 应答:记录 error_code / queue_ticket。
func MatchServiceJoinQueueHandler(player *gameobject.Player, response *match.JoinQueueResponse) {
	if response == nil {
		zap.L().Warn("nil JoinQueueResponse", zap.Uint64("player", player.ID))
		return
	}
	if tip := response.GetErrorMessage(); tip != nil || response.GetErrorCode() != 0 {
		zap.L().Warn("join queue rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("error_code", response.GetErrorCode()),
			zap.Uint32("tip_id", tip.GetId()),
			zap.Strings("tip_params", tip.GetParameters()),
		)
		return
	}
	zap.L().Info("join queue ok",
		zap.Uint64("player", player.ID),
		zap.String("queue_ticket", response.GetQueueTicket()),
	)
}

// MatchServiceWatchBattleHandler 处理 WatchBattle 应答:记录目标 battle_id / 错误提示。
func MatchServiceWatchBattleHandler(player *gameobject.Player, response *match.WatchBattleResponse) {
	if response == nil {
		zap.L().Warn("nil WatchBattleResponse", zap.Uint64("player", player.ID))
		return
	}
	if tip := response.GetErrorMessage(); tip != nil {
		zap.L().Warn("watch battle rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("tip_id", tip.GetId()),
			zap.Strings("tip_params", tip.GetParameters()),
		)
		return
	}
	zap.L().Info("watch battle ok",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetBattleId()),
	)
}
