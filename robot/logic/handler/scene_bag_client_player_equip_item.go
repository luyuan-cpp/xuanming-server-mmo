package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// EquipItemHandler:穿上。成功回人物背包 + 装备栏两份全量;被拒时响应体只有 error_message。
// 统一交给 RecordFeatureBody 按「来源消息号 + 序号」落快照。equip-smoke 按请求号认领回包(callDispatch),
// 回包在 RecvLoop 上经这里落快照后才唤醒等待方;序号游标只用来防止读到上一次的同号快照。
func SceneBagClientPlayerEquipItemHandler(player *gameobject.Player, response *scene.EquipItemResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerEquipItemMessageId, response)
}
