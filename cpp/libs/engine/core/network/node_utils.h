#pragma once
#include <optional>
#include <vector>
#include <grpc/impl/connectivity_state.h>
#include "node/system/node/node_util.h"
#include "engine/core/type_define/type_define.h"

using ServiceNodeList = std::array<NodeInfoListComp, eNodeType_ARRAYSIZE>;

NodeInfo &GetNodeInfo();
uint32_t GetZoneId();

// 为本玩家的请求挑一个 SceneManager 实例(换图、跨 zone 传送、镜像副本、紧急疏散、组队跟随都经这里)。
//
// - 候选是注册表里所有挂了 NodeInfo 的 SceneManager 实体(与旧实现 playerId % 已注册实例数同一个集合),
//   按通道状态分级挑,前一级有实例就不看后一级:READY → IDLE / CONNECTING → 挂了通道但 TRANSIENT_FAILURE /
//   SHUTDOWN → 没挂通道。规则本体见 scene_manager_selector::Pick,为什么这样分级见 node_utils.cpp。
// - 返回 entt::null 只有一种情况:注册表里一个 SceneManager 实体都没有(与旧实现相同)。调用方走各自"找不到
//   SceneManager"的立即失败分支,不要在同一次请求里原地重挑。
// - 注册表非空就一定挑出一个,哪怕全都不可达:那次调用以 UNAVAILABLE 进失败处理器或由看门狗收尾,与旧实现
//   相同;分级只在有更好选择时避开坏通道,不新增 null 窗口(跨 zone 交接 SET 之后拿到 null 要销毁 + 踢线)。
//   落到后两级时打一行节流的 LOG_WARN(每 10s 最多一行)。
// - 风险:挑中"没挂通道"的实体后,生成的发送函数取 CompletionQueue / stub 会断言。经 gRPC 发现的实体不会缺
//   通道,但 TCP 握手声明 node_type=SceneManager 可以造出这种实体,所以它只在前三级全空时才会被选,
//   详见 node_utils.cpp。
// - 候选集不变时同一玩家总落在同一实例;候选集一变可能换实例 —— scene_manager 没有按玩家存在单个
//   实例内存里的状态(请求去重、owner_epoch、交接标记都在 Redis),换实例不影响正确性。
// - 副作用:对每个挂了通道的候选调一次 GetState(try_to_connect=true),把 IDLE 的通道踢去重连;
//   落到兜底级时可能写一行告警日志。
// - 只在节点主循环线程调用(读 thread_local 注册表,告警节流状态也是 thread_local);不在 per-tick 路径上。
entt::entity GetSceneManagerEntity(Guid playerId);

namespace scene_manager_selector
{
	struct Candidate
	{
		entt::entity entity{entt::null};
		// 空 = 实体上没挂通道,不是某种连接状态。不要拿 GRPC_CHANNEL_SHUTDOWN 代替:两者分属不同的级,
		// 混成一级取模会让无通道实体在"还有 TRANSIENT_FAILURE 实例可兜底"时也可能被挑中。
		std::optional<grpc_connectivity_state> channelState;
	};

	// SceneManager 实例选择规则本体。纯函数:不碰注册表、不调 gRPC,单测直接喂状态
	// (cpp/tests/routing_identity_test/scene_manager_selector_test.cpp)。
	//
	//   ①  READY                                  首选
	//   ②  IDLE / CONNECTING                      没有 ① 时才用
	//   ③a 挂了通道,TRANSIENT_FAILURE / SHUTDOWN  ①② 都没有时兜底
	//   ③b 没挂通道(channelState 为空)            ①②③a 全空时才选
	//
	// 有 ①② 时绝不选 ③a / ③b;有 ③a 时绝不选 ③b。
	// 同一级内按打散后的 playerId 对该级实例数取模,序号沿用 candidates 的顺序;同一输入总得到同一实体。
	// 只有空表返回 entt::null,非空表一定返回其中一个实体。
	entt::entity Pick(const std::vector<Candidate> &candidates, Guid playerId);
}
