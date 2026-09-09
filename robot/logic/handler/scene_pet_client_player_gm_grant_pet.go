package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// GmGrantPetHandler:GM 发宝宝(核心线唯一获取入口),响应带新 pet_id 与全量列表。
// tip_id != 0 时服务器不填 pets,这里只记 tip,列表保持上一份不动。
func ScenePetClientPlayerGmGrantPetHandler(player *gameobject.Player, response *scene.GmGrantPetResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetGranted(response.PetId)
	player.SetPetList(response.Pets, tipId, game.ScenePetClientPlayerGmGrantPetMessageId)
}
