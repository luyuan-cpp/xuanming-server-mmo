package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// SummonPetHandler:出战成功后服务器回全量列表,客户端整体覆盖。
// tip_id != 0 时服务器不填 pets,这里只记 tip,列表保持上一份不动。
func ScenePetClientPlayerSummonPetHandler(player *gameobject.Player, response *scene.SummonPetResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetList(response.Pets, tipId, game.ScenePetClientPlayerSummonPetMessageId)
}
