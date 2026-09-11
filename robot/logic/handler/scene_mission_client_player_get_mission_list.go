package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneMissionClientPlayerGetMissionListHandler(player *gameobject.Player, response *scene.GetMissionListResponse) {
	RecordFeatureBody(player, game.SceneMissionClientPlayerGetMissionListMessageId, response)
}
