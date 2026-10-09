package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// UnequipItemHandler:卸下。成功回人物背包 + 装备栏两份全量;被拒时响应体只有 error_message。
// 与 EquipItemHandler 同一条记录路径(RecordFeatureBody)。
func SceneBagClientPlayerUnequipItemHandler(player *gameobject.Player, response *scene.UnequipItemResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerUnequipItemMessageId, response)
}
