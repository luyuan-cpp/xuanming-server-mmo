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
