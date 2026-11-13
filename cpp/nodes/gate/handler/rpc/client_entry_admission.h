#pragma once

// gate 客户端入口两道 login 相关闸的装配层:判定之后做什么(计非法包、回带请求 id 的信封、不往下转发)。
//
// 纯判定在 login_admission.h(IsGateSynthesizedOnly、EnterGameRoleSwitchRejectTip,零状态);调用点在
// client_session_handler.cpp 的 DispatchClientRpcMessage 与路由 lane 的发出钩子(gate_lane::SetSendHooks)。
// 从 client_session_handler.cpp 抽出来单独成文件,是为了把这两处接线编进 routing_identity_test 直接测
// (cpp/nodes/gate/tests/client_entry_admission_test.cpp):client_session_handler.cpp 拖着节点注册表、登录与场景入口,
// 进不了测试工程(2026-11-12,数据审计遗留「gate 两处接线只有纯函数用例」)。行为与抽出之前逐行相同。
//
// 全部只在 gate 的 EventLoop 线程上调用(会话表与 lane 都是 thread_local);不持有连接(AGENTS.md §11.7),
// 回包经会话里的 weak_ptr lock(gate_lane::SendEnvelope)或调用方当场传入的连接。

#include <cstdint>

#include "engine/core/type_define/type_define.h"
#include "muduo/net/TcpConnection.h"

class ClientRequest;
struct SessionInfo;

namespace gate_client_entry
{

// 客户端入口的消息号闸(DispatchClientRpcMessage 验签之后的第一道):号超出方法注册表、不在生成的上行白名单
// (IsClientMessageId)、或只能由 gate 合成(login_admission.h IsGateSynthesizedOnly:Disconnect 58、RequestGateMigration、
// RenewSession)时按非法包处置 —— SessionInfo::illegalPacketCount 计一次、**不回任何包**,计数到阈值
// (IllegalPacketCounter,环境变量 GATE_ILLEGAL_PACKET_THRESHOLD,缺省 50)就 forceClose 这条连接。
// 返回 true = 已处置,调用方随即返回:不进任何 lane、不往下转发。返回 false = 放行,什么都不做。
// 白名单校验排在验签之后、计入非法包闸门的理由见调用点(未认证客户端发不存在的号曾是一条日志放大 DoS)。
bool RejectUnauthorizedMessageId(SessionId sessionId, SessionInfo &session, const muduo::net::TcpConnectionPtr &conn,
								 uint32_t messageId);

// 发出前的登录过渡判定:路由模式是路由 lane 的发出钩子(BeforeSendFn,在请求真正发出那一刻、按收包顺序判),
// 直连模式由 RejectLoginTransitionBeforeDirectSend 在收包即发出时调。只读会话。
// 返回 0 = 放行;非 0 = 不转发,值是要回的 tip。现在只有 EnterGame 的换角闸(login_admission.h EnterGameRoleSwitchRejectTip,
// 纵深防御):会话已绑定角色、没 LeaveGame 就要进另一个角色时回 kLoginInProgress(请求体读不出角色时回
// kRequestMessageParseError),不计非法包、不断连;按 1024 采样打一行 WARN。
uint32_t LoginTransitionRejectTip(SessionId sessionId, const SessionInfo &session, const ClientRequest &request);

// 直连模式(收包即发出)的同一道闸:LoginTransitionRejectTip 不为 0 时回带请求 id 与原消息号的该码信封
// (gate_lane::SendEnvelope,确定未执行),返回 true,调用方随即返回 —— 不往下转发,也不执行会话状态变化
// (ApplyLoginTransition)。为 0 时返回 false,什么都不做。路由模式的同一判定由 lane 的发出钩子做,不经这里。
bool RejectLoginTransitionBeforeDirectSend(SessionId sessionId, SessionInfo &session, const ClientRequest &request);

} // namespace gate_client_entry
