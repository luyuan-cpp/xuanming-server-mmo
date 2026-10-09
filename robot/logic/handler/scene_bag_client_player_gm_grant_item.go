package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// GmGrantItemHandler:GM 发物(dev / test 才开),成功回人物背包全量;被拒时响应体只有 error_message。
// 与 EquipItemHandler 同一条记录路径(RecordFeatureBody)。
func SceneBagClientPlayerGmGrantItemHandler(player *gameobject.Player, response *scene.GmGrantItemResponse) {
	RecordFeatureBody(player, game.SceneBagClientPlayerGmGrantItemMessageId, response)
}
