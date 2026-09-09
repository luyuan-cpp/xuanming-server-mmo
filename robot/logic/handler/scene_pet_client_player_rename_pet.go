package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// RenamePetHandler:改名,响应带全量列表。
// tip_id != 0 时服务器不填 pets,这里只记 tip,列表保持上一份不动。
func ScenePetClientPlayerRenamePetHandler(player *gameobject.Player, response *scene.RenamePetResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetList(response.Pets, tipId, game.ScenePetClientPlayerRenamePetMessageId)
}
