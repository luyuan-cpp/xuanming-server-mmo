package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneMissionClientPlayerAcceptMissionHandler(player *gameobject.Player, response *scene.GetMissionListResponse) {
	RecordFeatureBody(player, game.SceneMissionClientPlayerAcceptMissionMessageId, response)
}
