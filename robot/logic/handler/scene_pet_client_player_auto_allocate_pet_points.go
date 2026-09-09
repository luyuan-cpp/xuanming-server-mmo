package handler

import (
	"proto/scene"
	"robot/logic/gameobject"
)

// AutoAllocatePetPointsHandler:自动加点只算不落,响应里没有列表,只有建议分配。
func ScenePetClientPlayerAutoAllocatePetPointsHandler(player *gameobject.Player, response *scene.AutoAllocatePetPointsResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetSuggestion(response.PetId, response.Suggested, tipId)
}
