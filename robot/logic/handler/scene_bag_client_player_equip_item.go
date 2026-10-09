package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// EquipItemHandler:穿上。成功回人物背包 + 装备栏两份全量;被拒时响应体只有 error_message。
// 统一交给 RecordFeatureBody 按「来源消息号 + 序号」落快照,equip-smoke 的等待方据此认领本次请求的响应。
func SceneBagClientPlayerEquipItemHandler(player *gameobject.Player, response *scene.EquipItemResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerEquipItemMessageId, response)
}
