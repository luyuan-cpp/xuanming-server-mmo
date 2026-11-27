#pragma once

#include <cstdint>
#include <string>

#include "type_define.h"

// 实体上"已放行的最高登录身份"(入场会话栅栏,docs/design/scene-enter-session-fence.md §5;判定在
// services/scene/player/system/enter_fence.h)。独立于 PlayerSessionSnapshotComp:入场第 2 步(RemovePlayerSessionSilently)会把
// 快照的会话置无效,身份组件不受影响,所以随后到达的陈旧请求仍有东西可比。
// 只在两处写(PlayerLifecycleSystem::RecordLoginIdentity):EnterScene 第 1 步绑定会话时、UpdateSessionDetail 改绑后。
// 会话有效才记会话号与 gate 实例;代次只取 max,从不回退。随实体销毁消失。纯运行时组件,不进 PlayerAllData、不序列化。
// 定义放在引擎层,因为推送也读它:按它记下的 gate 实例找会话所在的 gate(network_utils.h 的 ResolvePlayerGateEntity /
// BoundGateInstanceId,docs/design/scene-enter-chain-residuals.md §14.6)。
struct PlayerLoginIdentityComp
{
	SessionId sessionId{kInvalidSessionId};
	uint64_t loginGeneration{0};
	// 这条会话所在的 gate 进程(gate 的 node_uuid:gate 转来的入场取入站连接的握手身份,托管落点取 location 为断开那次登录
	// 记下的实例);空 = 未知(location 没记实例的托管落点)。会话号相同而实例不同的是另一个进程上的另一条会话。
	std::string gateInstanceId;
};
