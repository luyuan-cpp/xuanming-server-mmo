package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneBagClientPlayerGetBagHandler(player *gameobject.Player, response *scene.GetBagResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerGetBagMessageId, response)
}
