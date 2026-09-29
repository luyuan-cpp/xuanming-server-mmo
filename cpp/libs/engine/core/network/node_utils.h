#pragma once
#include <vector>
#include <grpc/impl/connectivity_state.h>
#include "node/system/node/node_util.h"
#include "engine/core/type_define/type_define.h"

using ServiceNodeList = std::array<NodeInfoListComp, eNodeType_ARRAYSIZE>;

NodeInfo &GetNodeInfo();
uint32_t GetZoneId();

// 为本玩家的请求挑一个 SceneManager 实例(换图、跨 zone 传送、镜像副本、紧急疏散、组队跟随都经这里)。
//
// - 只返回经 gRPC 接入(ConnectToGrpcNode 挂了通道)、且通道不在 TRANSIENT_FAILURE / SHUTDOWN 的实例;
//   有 READY 的只在 READY 里挑,没有才退到 IDLE / CONNECTING。规则本体见 scene_manager_selector::Pick,
//   为什么这样分档见 node_utils.cpp。
// - 返回 entt::null = 此刻没有可用实例(一个都没注册,或注册了但全部不可达)。调用方走各自"找不到
//   SceneManager"的立即失败分支,不要在同一次请求里原地重挑。
// - 候选集不变时同一玩家总落在同一实例;候选集一变可能换实例 —— scene_manager 没有按玩家存在单个
//   实例内存里的状态(请求去重、owner_epoch、交接标记都在 Redis),换实例不影响正确性。
// - 副作用:对每个候选的通道调一次 GetState(try_to_connect=true),把 IDLE 的通道踢去重连。
// - 只在节点主循环线程调用(读 thread_local 注册表);不在 per-tick 路径上。
entt::entity GetSceneManagerEntity(Guid playerId);

namespace scene_manager_selector
{
	struct Candidate
	{
		entt::entity entity{entt::null};
		grpc_connectivity_state channelState{GRPC_CHANNEL_SHUTDOWN};
	};

	// SceneManager 实例选择规则本体。纯函数:不碰注册表、不调 gRPC,单测直接喂状态
	// (cpp/tests/routing_identity_test/scene_manager_selector_test.cpp)。
	//
	//   READY                          首选
	//   IDLE / CONNECTING              没有 READY 时才用
	//   TRANSIENT_FAILURE / SHUTDOWN   永不选
	//
	// 同一档内按打散后的 playerId 对该档实例数取模,序号沿用 candidates 的顺序。
	// 空表或全部不可用返回 entt::null。
	entt::entity Pick(const std::vector<Candidate> &candidates, Guid playerId);
}
