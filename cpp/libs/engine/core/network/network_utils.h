#pragma once

#include <optional>
#include <string>
#include "core/network/grpc_client_call.h"
#include "entt/src/entt/entity/registry.hpp"
#include "type_define/type_define.h"
#include "core/utils/id/node_id_generator.h"
#include <utils/id/snow_flake.h>

// session_id 的布局:[gate node_id : 15 位][进程内序号 : 17 位],载体 uint32。序号位宽是会话号自己的常量,不借 snowflake 的
// kNodeBits:那个常量在 ENABLE_SNOWFLAKE_TESTING 下变成 9,会话号的安全上界会从 131071 静默掉到 511,默认的
// GateMaxConnections 直接拒启(docs/design/gate-connection-admission-control.md G12)。GetGateNodeId 按同一个位宽拆。
inline constexpr std::size_t kSessionSequenceBits = 17;
using SessionIdGenerator = TransientNodeCompositeIdGenerator<uint32_t, kSessionSequenceBits>;
static_assert(SessionIdGenerator::kSeqMask == 131071, "a gate issues at most 2^17 - 1 concurrent session ids");

void ParseIpPort(const std::string &input, std::string &ip, uint16_t &port);

std::string FormatIpAndPort(const std::string &ip, uint32_t port);

NodeId GetGateNodeId(SessionId session_id);

// Resolve the entt::entity of a Gate node in THIS process's GateNodeService
// registry, given a session_id originated by some Gate.
//
// Post uuid-refactor, entt::entity is no longer numerically equal to node_id
// (ConnectToTcpNode now uses registry.create() and keys nodes by uuid), so
// callers in non-Gate processes (primarily Scene) must not do
// `entt::entity{GetGateNodeId(session_id)}` — that would treat a business
// node_id as an arbitrary entity slot and miss the real entity.
//
// Scene only talks to same-zone Gates (cross-zone flows are redirected at the
// SceneManager layer), so (self_zone, node_id) is a valid composite key here.
// Returns std::nullopt if no matching Gate has been discovered locally.
//
// 只认 node_id:同一 node_id 短暂有两个 gate 进程、两者在本节点上都有实体时,取到的是第一个匹配的那个。
// 知道会话所在的 gate 进程时用下面的 ResolveSessionGateEntity / ResolvePlayerGateEntity。
std::optional<entt::entity> ResolveLocalZoneGateEntity(SessionId sessionId);

// 会话所在 gate 的实体。gateInstanceId 是这条会话所在的 gate 进程(会话绑定时记下的入站连接握手身份,
// PlayerLoginIdentityComp::gateInstanceId;改派票据抄下的是同一个值):非空时只按它找(NodeUtils::FindNodeEntityByUuid),
// 找不到返回空、不退回按 node_id —— 同一 node_id 上的另一个进程没有这条会话,推过去要么推不到,要么推给同号的另一名玩家
// (docs/design/scene-enter-chain-residuals.md §14.6)。空 = 未知,按会话号里的 node_id 找(ResolveLocalZoneGateEntity)。
std::optional<entt::entity> ResolveSessionGateEntity(SessionId sessionId, const std::string &gateInstanceId);

// 玩家实体这条会话(sessionId,通常取自 PlayerSessionSnapshotComp)所在 gate 的实体:身份组件(PlayerLoginIdentityComp)
// 记的会话就是这一条时按它记下的实例找,否则按 node_id(同 ResolveSessionGateEntity)。按玩家推送一律走这里。
std::optional<entt::entity> ResolvePlayerGateEntity(entt::entity player, SessionId sessionId);

// 玩家这条会话所在 gate 进程的实例 uuid,给 scene 自发的请求填 gate_instance_id(EnterSceneRequest、改派票据、战斗路由;
// 下游用它做 Kafka 目标校验):身份组件记的会话就是这一条且实例已知时用记下的实例(那个进程此刻不在本节点上也照用,由下游
// 按实例裁决,不换成同一 node_id 上的另一个进程);否则取按 node_id 找到的 gate 的 uuid;都没有返回空串。
std::string BoundGateInstanceId(entt::entity player, SessionId sessionId);
