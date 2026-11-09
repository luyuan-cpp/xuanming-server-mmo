#pragma once
#include <cstdint>
#include <optional>
#include <vector>
#include "node/system/node/node_util.h"
#include "engine/core/type_define/type_define.h"

// 只前向声明连接状态枚举,不包含 muduo_grpc/channel.h:本头经 node.h 进入大量翻译单元,另有 scene 业务、网络工具与
// 测试的 .cpp 直接包含,多数只用 GetNodeInfo / GetZoneId,包含 channel.h 会把 muduo TcpClient 与整套 muduo_grpc 头
// 带给它们(与 muduo_grpc_channel_slot.h 只前向声明 Channel 同理)。带固定底层类型的枚举在不透明声明处即是完整类型,std::optional 可以直接用。
// 底层类型必须与 channel.h 的定义一致:同时看到两处声明的翻译单元(node_utils.cpp、选点用例)不一致会直接编译报错。
// 要用枚举项(kReady 等)的翻译单元自己包含 muduo_grpc/channel.h。
namespace muduo_grpc
{
	enum class ConnectivityState : uint8_t;
}

// 所有者是 Node，构造时登记、析构时注销。
// 未登记时返回只读默认实例，不创建状态。
// 只在所属 loop 线程调用，返回引用不得越过所有者寿命。
const NodeInfo &GetNodeInfo();
void InstallLocalNodeInfo(const NodeInfo *info);
uint32_t GetZoneId();

// 为本玩家的请求挑一个 SceneManager 实例(换图、跨 zone 传送、镜像副本、紧急疏散、组队跟随都经这里)。
//
// - 候选必须同时拥有 NodeInfo 和未关闭的 muduo_grpc 通道(HasUsableGrpcClient:挂着 MuduoGrpcChannelPtr 且非 SHUTDOWN)。
//   按通道连接状态分级：READY → IDLE / CONNECTING → TRANSIENT_FAILURE / SHUTDOWN。
// - 没有具备传输的候选时返回 entt::null，由调用方明确拒绝；不把只有 NodeInfo 的实体当成服务。
// - 有传输但全部不可达时仍确定性挑一个，让本次调用经 UNAVAILABLE/既有看门狗收尾。
//   不增加原地重挑或重发，保留失败代表「结果未知」的语义。
// - 候选集不变时同一玩家总落在同一实例;候选集一变可能换实例 —— scene_manager 没有按玩家存在单个
//   实例内存里的状态(请求去重、owner_epoch、交接标记都在 Redis),换实例不影响正确性。
// - 副作用:对每个挂了通道的候选调一次 GetState(tryToConnect=true),把 IDLE 的通道踢去重连;
//   落到兜底级时可能写一行告警日志。
// - 只在节点主循环线程调用(读 thread_local 注册表,告警节流状态也是 thread_local;
//   muduo_grpc::Channel::GetState 还断言在通道所属 loop 线程);不在 per-tick 路径上。
entt::entity SelectSceneManagerEntity(Guid playerId);

namespace scene_manager_selector
{
	struct Candidate
	{
		entt::entity entity{entt::null};
		// 空 = 实体上没挂通道,不是某种连接状态。不要拿 ConnectivityState::kShutdown 代替:两者分属不同的级,
		// 混成一级取模会让无通道实体在"还有 TRANSIENT_FAILURE 实例可兜底"时也可能被挑中。
		std::optional<muduo_grpc::ConnectivityState> channelState;
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
