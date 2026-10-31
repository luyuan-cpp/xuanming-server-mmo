#pragma once

#include "type_define/type_define.h"
#include "network/rpc_session.h"

// 经 Gate 向客户端发送单向消息。只在所属节点的 EventLoop 线程调用，读取该线程的玩家与 Gate 注册表。
// 不等待客户端确认；玩家、路由或 RPC 会话缺失时跳过发送，调用方不能据此判断客户端已收到。
// 不保证跨发送路径的消息到达顺序。
// 依赖下游处理结果的消息，必须等对应 RPC 完成后再发送。

void SendMessageToClientViaGate(uint32_t messageId, const google::protobuf::Message &message, Guid playerId);
void SendMessageToClientViaGate(uint32_t messageId, const google::protobuf::Message &message,
                                entt::entity playerEntity);
// 已知 Gate RPC 会话与客户端会话号时直接发送，不再查找玩家实体或 Gate 路由。
// targetPlayerId 是 Gate 写 socket 前的身份栅栏（routing-identity-audit-20260908.md R13），
// 防止会话号复用后把消息发给另一个玩家。0 / kInvalidGuid 表示无身份的兼容路径，仅限确实无法取得身份时使用。
void SendMessageToClientSessionViaGate(uint32_t messageId, const google::protobuf::Message &message,
                                       RpcSession &gateRpcSession, SessionId clientSessionId, Guid targetPlayerId);

// 只发送给指定玩家的客户端，按 Gate 分组，并携带每个目标的会话号与玩家身份。
void BroadcastToPlayerClients(uint32_t messageId, const google::protobuf::Message &message,
                              const EntityUnorderedSet &playerEntities);
void BroadcastToPlayerClients(uint32_t messageId, const google::protobuf::Message &message,
                              const EntityVector &playerEntities);

// scenePlayerEntities 应覆盖场景内的玩家，仅用于发现需要联系的 Gate。
// 实际收件人由各 Gate 按 sceneId 筛选，不局限于传入集合中的玩家；玩家子集广播请用 BroadcastToPlayerClients。
void BroadcastToSceneClients(uint32_t messageId, const google::protobuf::Message &message, uint64_t sceneId,
                             const EntityUnorderedSet &scenePlayerEntities);
void BroadcastToSceneClients(uint32_t messageId, const google::protobuf::Message &message, uint64_t sceneId,
                             const EntityVector &scenePlayerEntities);

// 向当前线程 Gate 注册表中拥有 RpcSession 的 Gate 发广播，由各 Gate 转发给其上已绑定玩家的客户端会话（未登录的连接不收）。
// 覆盖范围取决于本节点已登记的 Gate，不保证覆盖其他节点或 zone 的客户端。
void BroadcastToAllClientsOnKnownGates(uint32_t messageId, const google::protobuf::Message &message);
