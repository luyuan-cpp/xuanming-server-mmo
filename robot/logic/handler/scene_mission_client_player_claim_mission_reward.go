package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneMissionClientPlayerClaimMissionRewardHandler(player *gameobject.Player, response *scene.GetMissionListResponse) {
	RecordFeatureBody(player, game.SceneMissionClientPlayerClaimMissionRewardMessageId, response)
}
