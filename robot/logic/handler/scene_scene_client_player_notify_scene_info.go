package handler

import (
	"proto/scene"
	"robot/logic/gameobject"
)

// NotifySceneInfo(31)是 SceneInfoC2S(43)的数据通道:43 的应答类型是 Empty,当前场景信息与分线目录
// (docs/design/world-channel-switch.md §3.1)都走这条推送。
//
// 这里只把整条消息交给 Player 留底,校验与等待由场景脚本做(channel_smoke_scenario.go 按序号等
// "这次请求带回来的那一份")。不打日志:压测模式下 AI 的移动动作每次都会触发它(logic/ai/robot_ai.go)。
func SceneSceneClientPlayerNotifySceneInfoHandler(player *gameobject.Player, response *scene.SceneInfoS2C) {
	player.SetSceneInfoPush(response)
}
