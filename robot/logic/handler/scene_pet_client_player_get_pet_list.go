package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// GetPetListHandler:拉全量宝宝列表(客户端零配表,维度名/资质/成长率都在里面)。
// tip_id != 0 时服务器不填 pets,这里只记 tip,列表保持上一份不动。
func ScenePetClientPlayerGetPetListHandler(player *gameobject.Player, response *scene.GetPetListResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetPetList(response.Pets, tipId, game.ScenePetClientPlayerGetPetListMessageId)
}
