package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// ResetPetPointsHandler:洗点,响应带点数返还后的全量列表。
// tip_id != 0 时服务器不填 pets,这里只记 tip,列表保持上一份不动。
func ScenePetClientPlayerResetPetPointsHandler(player *gameobject.Player, response *scene.ResetPetPointsResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetList(response.Pets, tipId, game.ScenePetClientPlayerResetPetPointsMessageId)
}
