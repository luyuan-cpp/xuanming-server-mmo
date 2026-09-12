package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func SceneBagClientPlayerSortBagHandler(player *gameobject.Player, response *scene.SortBagResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerSortBagMessageId, response)
}
