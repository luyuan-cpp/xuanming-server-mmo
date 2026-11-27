#pragma once

// gate 客户端连接存活检查的装配层(docs/design/gate-client-heartbeat.md):握手期限、应用层心跳、空闲超时。
//
// 纯逻辑(参数、心跳准入、判死、检查点堆)在 core 的 network/client_liveness.h,与 battle 直连共用;本文件只做副作用:
// 回包、通告参数、关连接、计数与日志。两道期限到了都只调 forceClose(),断线通知、断线保留、login 断线租约都由既有的
// 断开回调(GateClientSessionHandler::HandleConnectionDisconnection)处理,这里不加任何业务逻辑。
//
// 一条连接的生命周期:
//   建连(未验签)──ExpectHandshake──▶ 10s 内要验签,否则关
//   验签成功 ──OnTokenVerified──▶ 回包通告心跳参数,从此按空闲超时判死:30s 收不到任何一帧就关;
//                                 并且 60s 内要登录成功一次(kLoginDeadlineAfterVerify),否则关
//   客户端每 10s 一条心跳 ──HandleHeartbeat──▶ 回一条;任何一帧(NoteInboundFrame)都重新计时
//
// 状态是 gate loop 线程上的 thread_local(与路由 lane 同一做法);定时器与派发回调都不捕获任何对象,
// 也不持有连接(AGENTS.md §11.7:只经会话表里的 weak_ptr lock)。只能在 gate 的 EventLoop 线程上调用。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>

#include "muduo/base/Timestamp.h"
#include "muduo/net/TcpConnection.h"
#include "proto/common/base/message.pb.h"
#include "session/comp/session_info_comp.h"

namespace gate_client_heartbeat
{

// 扫描节拍(秒),GateRuntimeContext::clientLivenessSweepTimer 用。
inline constexpr double kSweepIntervalSeconds =
	std::chrono::duration<double>(client_liveness::kClientLiveness.checkPeriod).count();

// 验签之后多久之内必须登录成功一次(docs/design/gate-client-heartbeat.md §4「登录期限」),按「距验签的时长」算,发心跳不续命。
// 没有这一道时,验签之后不登录、只发心跳的连接能一直挂着;它们都算进 gate 人数,排队按人数放人,这些连接就挤掉了排队的名额。
// 60s:合法客户端验签一回来就发 Login(mmorpg-client GameClient.ConnectAndEnter、robot),正常几百毫秒,login 繁忙或抢账号锁
// 失败后重试也在十几秒内。比空闲超时(30s)长,验签之后静默的连接仍由空闲超时先关(heartbeat-idle-smoke 的 H3 量的是它)。
inline constexpr std::chrono::seconds kLoginDeadlineAfterVerify{60};

// 本连接收到了一帧(任何类型,含随后被拒绝的)。三个收包入口(验签、ClientRequest、心跳)找到会话之后各调一次。
// 还没开始跟踪的连接(验签之前)不读时钟。
inline void NoteInboundFrame(SessionInfo &session)
{
	if (session.liveness.tracked)
	{
		session.liveness.NoteInbound(std::chrono::steady_clock::now());
	}
}

// 新连接还没验签:登记握手期限。HandleConnectionEstablished 插入会话之后调用;dev 免验签的会话建连即已验证,不调。
void ExpectHandshake(SessionId sessionId, const SessionInfo &session, client_liveness::SteadyTime now);

// 验签成功、即将回 ClientTokenVerifyResponse{success = true}:回包填心跳参数,并从这一刻起按空闲超时判死这条连接,
// 同时登记登录期限(kLoginDeadlineAfterVerify)。
// 两条成功路径(正常验签、dev 免验签)都调;dev 下每条验签请求都答,重复调用只填参数,不重复登记。
// 已验签的连接再发验签请求不回包、计非法包(client_session_handler.cpp DispatchTokenVerify),不走到这里。
void OnTokenVerified(SessionId sessionId, SessionInfo &session, ClientTokenVerifyResponse &response,
					 client_liveness::SteadyTime now);

// ProtobufDispatcher 的回调(GateClientSessionHandler 构造时注册)。读一次时钟,转给 HandleHeartbeat。
void OnHeartbeatRequest(const muduo::net::TcpConnectionPtr &conn, const std::shared_ptr<ClientHeartbeatRequest> &request,
						muduo::Timestamp receiveTime);

// 收到一条心跳,时刻由调用方给(单测接缝)。处置见设计 §5.2:
//   找不到会话 / 未验签:forceClose;本窗口已应答满:不回、计一次非法包,到阈值 forceClose;
//   其余回 ClientHeartbeatResponse{seq}。
void HandleHeartbeat(const muduo::net::TcpConnectionPtr &conn, const ClientHeartbeatRequest &request,
					 client_liveness::SteadyTime now);

// 扫描:定时器每个节拍调一次;单测直接传时刻。握手超时、静默满阈值、到了登录期限仍未登录成功的连接 forceClose,之后照常走断开回调。
// 顺带每 30s 检查一次汇总行,累计计数有变化才打。
void SweepLiveness(client_liveness::SteadyTime now);

// 进程级累计计数(gate loop 线程,单调不减)。供汇总日志与单测读增量。
struct Counters
{
	uint64_t answered{0};          // 应答的心跳
	uint64_t excess{0};            // 超出窗口上限、不应答的心跳
	uint64_t beforeVerify{0};      // 验签之前发来、被断开的心跳
	uint64_t tracked{0};           // 开始按空闲超时判死的连接(每条连接一次)
	uint64_t idleClosed{0};        // 静默满阈值、被关闭的连接
	uint64_t handshakeTimeouts{0}; // 建连满握手期限仍未验签、被关闭的连接
	uint64_t loginTimeouts{0};     // 验签满登录期限仍未登录成功、被关闭的连接
	uint64_t deferred{0};          // 本端 loop 迟到而推迟的判定(三道期限合计)

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();

// 检查点堆的大小(含已关闭会话、已验签连接尚未到期的检查点),只作观测。
std::size_t PendingChecks();

// 本端 loop 是否刚卡过(client_liveness.h LoopStallWatch;节拍就是 SweepLiveness 每秒一次的扫描,还没扫过也算卡过)。
// 未验签淘汰(connection_admission.cpp)这时不淘汰:验签请求可能还在接收缓冲里没读,与握手期限这一侧的「迟到只推迟」同一口径。
bool LoopRecentlyStalled(client_liveness::SteadyTime now);

} // namespace gate_client_heartbeat
