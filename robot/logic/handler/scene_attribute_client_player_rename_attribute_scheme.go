package handler

import (
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

// RenameAttributeSchemeHandler:属性 RPC 的响应一律带全量面板(设计文档
// player-attribute-allocation.md §4.1 —— 写操作回全量,客户端整体覆盖)。
// tip_id != 0 时服务器不填 panel,这里只记 tip,面板保持上一份不动。
func SceneAttributeClientPlayerRenameAttributeSchemeHandler(player *gameobject.Player, response *scene.RenameAttributeSchemeResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetAttributePanel(response.Panel, tipId, game.SceneAttributeClientPlayerRenameAttributeSchemeMessageId)
}
