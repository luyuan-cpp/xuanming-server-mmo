package handler

import (
	"go.uber.org/zap"

	"proto/scene"
	"robot/logic/gameobject"
	"robot/pkg"
)

func SceneSceneClientPlayerNotifyEnterSceneHandler(player *gameobject.Player, response *scene.EnterSceneS2C) {
	if response.GetSceneInfo() == nil {
		return
	}
	player.SetSceneInfo(response.GetSceneInfo().GetSceneId(), response.GetSceneInfo().GetSceneConfigId())
	player.SignalSceneReady()
	// 成功进场:跟随重定向的跳数清零。gate 排空迁移可能让同一个玩家换好几次 gate,
	// 每次都已成功进场,不该累计到 3 跳熔断(docs/design/gate-drain-session-migration.md §7.3)。
	if gc := pkg.Clients.Get(player.ID); gc != nil {
		gc.NoteSceneEntered()
	}
	zap.L().Info("notify enter scene",
		zap.Uint64("player", player.ID),
		zap.Uint64("scene_id", response.GetSceneInfo().GetSceneId()),
		zap.Uint32("scene_config_id", response.GetSceneInfo().GetSceneConfigId()),
	)
}
