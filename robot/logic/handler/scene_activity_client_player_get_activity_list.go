package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneActivityClientPlayerGetActivityListHandler(player *gameobject.Player, response *scene.GetActivityListResponse) {
	RecordFeatureBody(player, game.SceneActivityClientPlayerGetActivityListMessageId, response)
}
