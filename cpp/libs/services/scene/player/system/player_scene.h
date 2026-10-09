#pragma once

#include "entt/src/entt/entity/entity.hpp"

#include <cstdint>

class PlayerSceneSystem
{
public:
	static void HandleEnterScene(entt::entity player, entt::entity scene);

	// RequestEnterMirrorScene asks SceneManager to create a mirror of the
	// player's current scene and then route the player into it. The
	// mirror is created with:
	//   - source_scene_id  = player's current scene_id (drives co-location;
	//     the new mirror lands on the same node when feasible)
	//   - mirror_config_id = caller-provided template id (>0; SceneInfoComp
	//     downstream filters mirrors via mirror_config_id() > 0)
	//   - creator_ids      = [player_id]; SceneManager echoes these in
	//     CreateSceneResponse so the response handler can dispatch the
	//     follow-up EnterScene without per-call request state
	//   - scene_conf_id    = the source's scene_config_id (template) so the
	//     mirror inherits the same map definition
	//   - scene_type       = INSTANCE (mirrors are not main-world channels)
	//
	// Returns true if the request was dispatched (CreateScene fired). False
	// when the prerequisites failed (no current scene, no SceneManager node,
	// missing player session). Failure to dispatch is logged at WARN; an
	// async failure on the SceneManager side is logged at ERROR by the
	// AsyncSceneManagerCreateScene handler.
	//
	// mirrorConfigId must be > 0. Pass 0 only if the intent is "clone
	// everything from source" (no template), which goes through the
	// source-clone refusal path on SceneManager and is currently NOT
	// supported by this helper (callers always pick a mirror template).
	static bool RequestEnterMirrorScene(entt::entity player, uint32_t mirrorConfigId);

	// 把玩家当前场景的信息推给客户端(NotifySceneInfo,31 号)。SceneInfoC2S(43 号)的应答类型是 Empty,
	// 数据只走这条推送。withChannelDirectory(= 请求里的 with_channel_directory)为 true 且当前场景是
	// 大世界分线时,附带该地图的分线目录(SceneInfoS2C.channel_directory,docs/design/world-channel-switch.md §5):
	//   - 目录是 scene_manager 发布到 zone Redis 的读模型,这里只读转发,不做任何放行判断
	//     (切线请求仍由 scene_manager 在 EnterScene 里重新校验);
	//   - 读目录是一次异步 GET,所以大世界线上的推送晚于本函数返回;副本 / 镜像里、zone Redis
	//     未连接、或读命令发不出去时同步推送且不带目录;
	//   - withChannelDirectory 为 false 时同步推送、不读 Redis、不带目录,开销与加目录之前完全相同
	//     (压测机器人把 SceneInfoC2S 当高频动作在发,不能让它平白多出一次 Redis 往返);
	//   - 目录键不存在 / 读失败 / 解析失败 / 目录写的不是当前地图:照样推送,只是不带目录。
	//     客户端靠这条推送结束等待,所以"读不到目录"不能变成"不推"。
	// 不推送的情况只有:实体无效或玩家不在任何场景(记日志,与原 handler 行为一致);异步应答回来时
	// 实体已销毁、实体槽位已被别的玩家复用、或玩家已换到别的场景(客户端进新场景后会重新请求)。
	// 只在 scene 节点 loop 线程调用(hiredis 回调也在该线程)。由客户端请求触发,不在 per-tick 路径上。
	static void SendSceneInfo(entt::entity player, bool withChannelDirectory);
};
