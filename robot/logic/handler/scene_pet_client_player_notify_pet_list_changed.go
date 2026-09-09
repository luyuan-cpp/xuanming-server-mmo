package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// NotifyPetListChangedHandler:服务器主动推的全量宝宝列表(主人升级带动宝宝升级、
// 战斗结算回写残血等)。推送没有 error_message,tip 记 0。
func ScenePetClientPlayerNotifyPetListChangedHandler(player *gameobject.Player, response *scene.PetListChangedS2C) {
	if response == nil {
		return
	}
	player.SetPetList(response.Pets, 0, game.ScenePetClientPlayerNotifyPetListChangedMessageId)
}
