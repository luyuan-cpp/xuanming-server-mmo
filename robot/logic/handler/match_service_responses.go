package handler

// match_service 客户端应答 handler(手写,非生成器产物)。
// battle-smoke 场景发 JoinQueue / WatchBattle 后,服务端的应答以同 message id
// 回到客户端;这里只做诊断日志:错误提示打 Warn,成功打 Info(带 ticket / battle_id)。
// RequestBattleTicket 应答除日志外还要唤醒等待直连的场景(见下)。

import (
	"go.uber.org/zap"

	"proto/battle"
	"proto/match"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// MatchService 应答的分发登记。生成的 message_body_handler.go 只收服务名含
// "ClientPlayer" / "GamePlayer" 的服务(tools/proto_generator/protogen/internal/generator/go/
// robot_case.go 的 isRelevantService),MatchService 不在其列,生成表里永远不会出现这三条;
// 不手改生成物,在本手写文件里 init() 补登记(包级 var 先于 init 初始化,map 此时已建好),
// 消息号仍用生成常量。少了这一步,三个 handler 是死代码,补签拿到的票据永远唤不醒直连场景。
func init() {
	messageHandlers[game.MatchServiceJoinQueueMessageId] = unmarshalAndCall(MatchServiceJoinQueueHandler)
	messageHandlers[game.MatchServiceWatchBattleMessageId] = unmarshalAndCall(MatchServiceWatchBattleHandler)
	messageHandlers[game.MatchServiceRequestBattleTicketMessageId] = unmarshalAndCall(MatchServiceRequestBattleTicketHandler)
}

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

// MatchServiceRequestBattleTicketHandler 处理丢票补签应答
// (docs/design/turn-based-battle-server.md §18 D25;改道见 client-rpc-router.md D33:
// 客户端 → MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket,应答里的
// error_message / assignment 是 battle 的裁决经 match 原样透传):成功时把补签的落点分配
// 当作一份新的 BattleAssigned 记录下来(唤醒等待直连的脚本化场景),失败只记日志
// (tip 由调用方按需断言)。
func MatchServiceRequestBattleTicketHandler(player *gameobject.Player, response *battle.RequestBattleTicketResponse) {
	if response == nil {
		zap.L().Warn("nil RequestBattleTicketResponse", zap.Uint64("player", player.ID))
		return
	}
	if response.GetErrorMessage().GetId() != 0 {
		zap.L().Warn("request battle ticket rejected",
			zap.Uint64("player", player.ID),
			zap.Uint32("tip", response.GetErrorMessage().GetId()),
			zap.Strings("tip_params", response.GetErrorMessage().GetParameters()))
		return
	}
	player.SignalBattleAssigned(response.GetAssignment())
	zap.L().Info("request battle ticket ok",
		zap.Uint64("player", player.ID),
		zap.Uint64("battle_id", response.GetAssignment().GetBattleId()),
		zap.String("role", response.GetAssignment().GetRole().String()))
}
