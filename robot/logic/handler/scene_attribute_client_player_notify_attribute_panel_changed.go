package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// 服务器主动推面板(升级 / GM 改等级 / 外部加成变化):与响应同一条覆盖路径。
func SceneAttributeClientPlayerNotifyAttributePanelChangedHandler(player *gameobject.Player, response *scene.AttributePanelChangedS2C) {
	if response == nil {
		return
	}
	player.SetAttributePanel(response.Panel, 0, game.SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId)
}
