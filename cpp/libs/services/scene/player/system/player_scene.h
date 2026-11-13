#pragma once

#include "entt/src/entt/entity/entity.hpp"
#include "engine/core/utils/registry/scene_entity.h"
#include "engine/core/type_define/type_define.h"
#include "core/network/grpc_client_call.h"

#include <cstdint>
#include <optional>
#include <string>

namespace scene_manager
{
class CreateSceneRequest;
class CreateSceneResponse;
}

class PlayerSceneSystem
{
public:
	// 同步通知不能把旧流程带进新的玩家/会话身份。未登记索引的独立 actor 允许保持未登记;
	// 生产入口另要求索引归本实体。requestSession 用于加载尚未绑定快照时守住预登记会话。
	class EntryOwnershipGuard
	{
	public:
		explicit EntryOwnershipGuard(entt::entity player, SessionId requestSession = 0);
		[[nodiscard]] bool IsCurrent() const;
	private:
		struct Snapshot
		{
			Guid playerId{0};
			entt::entity indexedPlayer{entt::null};
			SessionId sessionId{0};
			Guid snapshotPlayerId{0};
			std::optional<Guid> sessionOwner;
			SessionId loginSession{0};
			uint64_t loginGeneration{0};
			uint32_t homeZoneId{0};
			uint64_t ownerEpoch{0};
			bool operator==(const Snapshot&) const = default;
		};
		static std::optional<Snapshot> Read(entt::entity player, SessionId requestSession);
		entt::entity player_;
		SessionId requestSession_;
		std::optional<Snapshot> snapshot_;
	};

	// 只读解析业务场景号;不存在时返回 null,不把业务号当成本地句柄。
	[[nodiscard]] static SceneEntity FindSceneById(uint64_t sceneId);
	// 纯校验:场景须存在于本节点、身份非零;不改会话、归属、AOI 或场景绑定。
	// creators 只用于镜像自动进场回显,不承担准入语义(两版同基线,见 PARITY B5)。
	// GUID 重载供尚未创建实体的加载入口预检,实体重载还要求真实有效且非零的玩家身份。
	[[nodiscard]] static uint32_t CheckEnterScene(Guid playerId, SceneEntity scene);
	[[nodiscard]] static uint32_t CheckEnterScene(entt::entity player, SceneEntity scene);

	// 进场请求背后的客户端是不是玩家已在场景里时的那一个。只影响「已在目标场景」这一种情况怎么处理。
	enum class EntryClient : uint8_t
	{
		kSame, // 同一次登录的重复路由:客户端已有入场画面,幂等跳过
		kNew,  // 换了客户端(断线保留内重连、顶号、gate 排空迁移):只给新客户端重发入场画面,见 HandleEnterScene
	};

	// true 表示已绑定目标场景(含幂等命中);输入无效或场景不存在返回 false。
	// 调用方只有成功后才可写登录态、触发后续落地逻辑。
	// 已在目标场景、client 为 kNew 时:只给新客户端重发入场画面(入场通知、自身的创建、当前显示中的目标),
	// 场景绑定、格子与 AOI 关系都不动,所以别的玩家看不到他先消失再出现。不在目标场景时 client 不起作用,照常完整入场。
	[[nodiscard]] static bool HandleEnterScene(entt::entity player, SceneEntity scene,
											   EntryClient client = EntryClient::kSame);

	// 本线程累计的「已在目标场景、给新客户端重发入场画面」次数,只增不减。诊断与单测用:
	// 单测宿主没有 gate,看不到发出的消息。
	[[nodiscard]] static uint64_t EntrySnapshotResendCount();

	// AOI 先用仍在的 Hex / SceneEntityComp 清理格子和观察者,再成对摘除场景绑定与 Hex。
	// 否则重连复用实体、落点 Hex 未变时不会重新入格;无效实体或不在场景里时 no-op。
	static void LeaveCurrentScene(entt::entity player);

	// RequestEnterMirrorScene asks SceneManager to create a mirror of the
	// player's current scene and then route the player into it. The
	// mirror is created with:
	//   - source_scene_id  = player's current scene_id (drives co-location;
	//     the new mirror lands on the same node when feasible)
	//   - mirror_config_id = caller-provided template id (>0; SceneInfoComp
	//     downstream filters mirrors via mirror_config_id() > 0)
	//   - creator_ids      = [player_id](镜像自动进场的创建者回显 / 归属提示;scene_manager 仍会回显,本节点按调用捕获身份认领,不用于准入)
	//   - scene_conf_id    = the source's scene_config_id (template) so the
	//     mirror inherits the same map definition
	//   - scene_type       = INSTANCE (mirrors are not main-world channels)
	//
	// 受理后当场登记镜像令牌并提交 CreateScene，自动进场由任务在同一 Scene loop 完成。
	// mirrorConfigId 必须非 0。预检未通过或停机时返回 false，不登记在途状态。
	// 提交接缝遵守 GrpcDone 契约：请求借用到提交返回，完成在所属 loop 异步交付。
	using MirrorCreateSubmit = std::function<void(entt::entity, const ::scene_manager::CreateSceneRequest&,
		GrpcDone<::scene_manager::CreateSceneResponse>)>;
	static bool RequestEnterMirrorScene(entt::entity player, uint32_t mirrorConfigId, MirrorCreateSubmit submit = {});

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

	// 分线目录的 Redis 键 world_channel_directory:{ctl:<zone>}:<scene_config_id>(STRING,值是 SceneChannelDirectory 的序列化字节)。
	// 跨语言契约:与 go/scene_manager/internal/logic/world_channel_directory.go 的 worldChannelDirectoryKey 渲染出来的键逐字节相同,
	// 改一边必须改另一边;tag 由族表生成(docs/design/redis-cluster-all-domains-20261110.md §3)。
	[[nodiscard]] static std::string WorldChannelDirectoryKey(uint32_t zoneId, uint32_t sceneConfigId);
};
