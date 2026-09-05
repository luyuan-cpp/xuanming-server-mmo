package handler

import (
	"proto/scene"
	"robot/logic/gameobject"
)

// 自动加点只算不落:响应给的是"建议的目标已分配值",不改服务器状态,也不带面板。
func SceneAttributeClientPlayerAutoAllocateAttributePointsHandler(player *gameobject.Player, response *scene.AutoAllocateAttributePointsResponse) {
	if response == nil {
		return
	}
	var tipId uint32
	if response.ErrorMessage != nil {
		tipId = response.ErrorMessage.Id
	}
	player.SetAttributeSuggestion(response.PoolId, response.Suggested, tipId)
}
