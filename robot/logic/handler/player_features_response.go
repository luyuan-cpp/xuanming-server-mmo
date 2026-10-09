package handler

import (
	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// HandleFeatureMessage is explicitly used by features-smoke. Existing robot
// modes keep their receive behavior. It consumes envelope failures before body
// decoding, never treating a rejected RPC as a successful zero-valued list.
func HandleFeatureMessage(player *gameobject.Player, message *base.MessageContent) bool {
	if player == nil || message == nil {
		return false
	}
	var response proto.Message
	switch message.MessageId {
	case game.SceneBagClientPlayerGetBagMessageId:
		response = &scene.GetBagResponse{}
	case game.SceneBagClientPlayerSortBagMessageId:
		response = &scene.SortBagResponse{}
	case game.SceneBagClientPlayerEquipItemMessageId:
		// 穿 / 脱 / GM 发物(equipment-attributes.md §3.2):供 features-smoke 这条收包路径以后发这三条消息时用,
		// 与 GetBag 同一套「信封错误先于解包」的口径。equip-smoke **不经过这里** —— 它走 prepareBehaviorClient
		// 的通用分发(信封错误只打日志,随后 stub → RecordFeatureBody 把全零响应落成 failure)。
		response = &scene.EquipItemResponse{}
	case game.SceneBagClientPlayerUnequipItemMessageId:
		response = &scene.UnequipItemResponse{}
	case game.SceneBagClientPlayerGmGrantItemMessageId:
		response = &scene.GmGrantItemResponse{}
	case game.SceneActivityClientPlayerGetActivityListMessageId:
		response = &scene.GetActivityListResponse{}
	case game.SceneMissionClientPlayerGetMissionListMessageId,
		game.SceneMissionClientPlayerAcceptMissionMessageId,
		game.SceneMissionClientPlayerClaimMissionRewardMessageId:
		response = &scene.GetMissionListResponse{}
	default:
		return false
	}
	if tip := message.GetErrorMessage().GetId(); tip != 0 {
		player.SetFeatureSnapshot(message.MessageId, nil, tip, "gate rejected feature request")
		return true
	}
	if err := proto.Unmarshal(message.SerializedMessage, response); err != nil {
		player.SetFeatureSnapshot(message.MessageId, nil, 0, "invalid feature response encoding")
		return true
	}
	RecordFeatureBody(player, message.MessageId, response)
	return true
}

// RecordFeatureBody also backs the generated typed handler seams. No response
// or serialized packet is logged: authentication data stays outside reports.
func RecordFeatureBody(player *gameobject.Player, messageID uint32, response proto.Message) {
	if player == nil {
		return
	}
	var tip uint32
	var failure string
	switch body := response.(type) {
	case *scene.GetBagResponse:
		tip = body.GetErrorMessage().GetId()
		if body.GetBag().GetLayout() == nil {
			failure = "bag response has no layout"
		}
	case *scene.SortBagResponse:
		tip = body.GetErrorMessage().GetId()
		if body.GetBag().GetLayout() == nil {
			failure = "sort response has no layout"
		}
	case *scene.EquipItemResponse:
		// 穿 / 脱成功必须同时带回人物背包与装备栏两份全量,缺任何一份都不能算成功:
		// gate 级拒绝(限流 / GM 闸)被通用分发器解出来的就是一份全零响应,要落成 failure 而不是「空包」。
		// 业务拒绝(tip != 0)时两个包本来就不填,函数末尾统一改写成 server rejected。
		tip = body.GetErrorMessage().GetId()
		if body.GetBag().GetLayout() == nil || body.GetEquipment().GetLayout() == nil {
			failure = "equip response has no bag or equipment layout"
		}
	case *scene.UnequipItemResponse:
		tip = body.GetErrorMessage().GetId()
		if body.GetBag().GetLayout() == nil || body.GetEquipment().GetLayout() == nil {
			failure = "unequip response has no bag or equipment layout"
		}
	case *scene.GmGrantItemResponse:
		tip = body.GetErrorMessage().GetId()
		if body.GetBag().GetLayout() == nil {
			failure = "gm grant item response has no layout"
		}
	case *scene.GetMissionListResponse:
		tip = body.GetErrorMessage().GetId()
	case *scene.GetActivityListResponse:
		tip = body.GetErrorMessage().GetId()
	default:
		failure = "unexpected feature response type"
	}
	if tip != 0 {
		failure = "server rejected feature request"
	}
	player.SetFeatureSnapshot(messageID, response, tip, failure)
}
