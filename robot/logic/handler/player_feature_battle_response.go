package handler

import (
	"google.golang.org/protobuf/proto"
	"proto/battle"
	"proto/common/base"
	"proto/match"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// 仅 features-smoke 显式战斗模式调用，其他模式保留已有收包行为。
// 排队/自动战斗拒绝与通知解码失败写入专用 waiter，绝不把空错误包当成功。
func HandleFeatureBattleMessage(player *gameobject.Player, message *base.MessageContent) bool {
	if player == nil || message == nil {
		return false
	}
	var response proto.Message
	switch message.MessageId {
	case game.MatchServiceJoinQueueMessageId:
		response = &match.JoinQueueResponse{}
	case game.BattleClientPlayerSetAutoBattleMessageId:
		response = &battle.SetAutoBattleResponse{}
	case game.BattleClientPlayerNotifyBattleStartMessageId:
		response = &battle.BattleStartS2C{}
	case game.BattleClientPlayerNotifyTurnResultMessageId:
		response = &battle.TurnResultS2C{}
	case game.BattleClientPlayerNotifyBattleEndMessageId:
		response = &battle.BattleEndS2C{}
	default:
		return false
	}
	fail := func(tip uint32, reason string) {
		player.SetFeatureSnapshot(message.MessageId, nil, tip, reason)
	}
	if tip := message.GetErrorMessage().GetId(); tip != 0 {
		fail(tip, "gate rejected feature battle request")
		return true
	}
	if err := proto.Unmarshal(message.SerializedMessage, response); err != nil {
		fail(0, "invalid feature battle response encoding")
		return true
	}
	switch body := response.(type) {
	case *match.JoinQueueResponse:
		if tip := body.GetErrorMessage().GetId(); tip != 0 {
			fail(tip, "join queue rejected")
			return true
		}
		if body.GetErrorCode() != 0 {
			fail(body.GetErrorCode(), "join queue rejected")
			return true
		}
		if body.GetQueueTicket() == "" {
			fail(0, "join queue response has no ticket")
			return true
		}
	case *battle.SetAutoBattleResponse:
		if tip := body.GetErrorMessage().GetId(); tip != 0 {
			fail(tip, "auto battle rejected")
			return true
		}
	case *battle.BattleStartS2C:
		if body.GetBattleId() == 0 || body.GetState().GetBattleId() != body.GetBattleId() {
			fail(0, "battle start has invalid identity")
			return true
		}
		if current := player.GetBattleId(); current != 0 && current != body.GetBattleId() {
			fail(0, "unexpected second battle start")
			return true
		}
		player.SignalBattleStart(body.GetBattleId())
	case *battle.TurnResultS2C:
		if body.GetBattleId() == 0 || body.GetRoundIndex() == 0 {
			fail(0, "battle turn has invalid identity or round")
			return true
		}
		if body.GetBattleId() != player.GetBattleId() {
			return true
		}
		if previous, ok := player.FeatureResponse(message.MessageId, 0); ok {
			if last, ok := previous.Response.(*battle.TurnResultS2C); ok &&
				last.GetBattleId() == body.GetBattleId() && last.GetRoundIndex() >= body.GetRoundIndex() {
				return true
			}
		}
		player.AddTurnResult()
	case *battle.BattleEndS2C:
		if body.GetBattleId() == 0 {
			fail(0, "battle end has no identity")
			return true
		}
		if body.GetBattleId() != player.GetBattleId() {
			return true
		} // 登录补推旧局不能完成本局 waiter。
		if body.GetSettlement().GetPlayerId() != player.ID ||
			body.GetSettlement().GetBattleId() != body.GetBattleId() {
			fail(0, "battle end settlement has wrong owner or battle")
			return true
		}
		player.SignalBattleEnd(body.GetBattleId(), int32(body.GetOutcome()))
	}
	player.SetFeatureSnapshot(message.MessageId, response, 0, "")
	return true
}
