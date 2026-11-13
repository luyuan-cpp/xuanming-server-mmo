#pragma once

// gate 为仍然连着的连接续租它在 login 的按连接记录(docs/design/gate-online-identity.md §7.3、§10.4)。
//
// login 的登录态、设备集成员与绑定栅栏都有寿命。寿命只是「连接断了、Disconnect 又没送到」时的回收兜底,不该比活着的连接
// 先到期:登录态到期后,这条连接不再计入账号的设备数上限。所以连接活着期间由 gate 定期续租:
//   登录成功 ──NoteLoginSucceeded──▶ 下一拍向 login 发 ClientPlayerLogin.RenewSession
//                                    (只有 gate 合成;连接身份走签名的 SessionDetails,路由模式经路由服,直连模式直发 login)
//   ──应答 renewed──▶ 按 login 给的间隔(登录态寿命的三分之一)排下一次。寿命只在 login 配,gate 不另配
//   ──应答 not renewed──▶ login 已没有这条连接的记录(离开游戏、已按断线处理):停止,直到这条连接再次登录成功
//   ──调用失败 / 没发出──▶ kRetryBackoff 之后再试
// 连接断开不需要通知本模块:到期项按会话号与会话代次核对,对不上就丢弃。
//
// 续租失败不影响在线玩家:连续失败超过登录态寿命的后果与没有续租时相同(这条连接不再计入设备数)。
//
// 本模块不碰路由 lane、不编码帧、不选节点:发请求是装配层注入的函数(GateClientSessionHandler 构造时注入,同
// gate_drain_migration::SetRequestSender 的做法),单测注入替身。状态是 gate loop 线程上的 thread_local;回调只捕获会话号与
// 会话代次,不捕获对象,也不持有连接(AGENTS.md §11.7)。只能在 gate 的 EventLoop 线程上调用。
//
// 这里没有「重连 / 连上某类节点」时的业务逻辑:续租只由本连接的登录成功与定时器驱动。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>

#include "engine/core/type_define/type_define.h"
#include "proto/login/login.pb.h"
#include "session/comp/session_info_comp.h"

namespace gate_login_renewal
{

using SteadyTime = std::chrono::steady_clock::time_point;

// Tick 的节拍(秒),GateRuntimeContext::loginRenewalTimer 用。
inline constexpr double kTickIntervalSeconds = 1.0;
// 每拍至多发起的续租请求。login 故障恢复后所有连接同时到期时,按这个速率摊开,不在一拍里打出去。
// 5 万条连接的 gate 约 4 分钟补完,远小于「间隔是寿命三分之一」留下的余量。
inline constexpr uint32_t kMaxRequestsPerTick = 200;
// 在途请求的上限。应答迟迟不回(login 慢、路由服慢)时不再加压,等在途的回来或超时。
inline constexpr uint64_t kMaxInFlight = 2ull * kMaxRequestsPerTick;
// 一次续租没成(调用失败、没发出)之后,隔这么久再试。
inline constexpr std::chrono::seconds kRetryBackoff{30};
// login 给的间隔的合法范围,越界就夹回来:下限防止异常应答让一条连接每拍都续,上限防止间隔大到等于不续。
inline constexpr std::chrono::seconds kMinInterval{5};
inline constexpr std::chrono::seconds kMaxInterval{24 * 60 * 60};

// 续租的应答,两种出站模式统一成这一种。delivered = false:调用失败或应答不可解析(结果未知,按失败退避)。
struct RequestReply
{
	bool delivered{false};
	loginpb::RenewSessionResponse response;
};
// receivedAt = 应答到达时刻(单调时钟),由出口在回调点读取后传入:下一次续租从这里起算,本模块自己不读时钟。
using ReplyCallback = std::function<void(const RequestReply &reply, SteadyTime receivedAt)>;

// 发 RenewSession 的出口。返回 true = 已发出,done 之后恰好被调用一次(可能同步调用);
// false = 没发出,done 不会被调用。
using RequestSender = bool (*)(SessionId sessionId, const SessionInfo &session, ReplyCallback done);

// 装配:gate 启动时注入一次,单测注入替身;传 nullptr 撤销。
void SetRequestSender(RequestSender sender);

// 本连接收到了一次成功的 Login 回包(与置 SessionInfo::loginSucceeded 同一处调用)。没有排着的续租就排一次、下一拍发出:
// 第一次续租同时向 login 取间隔。已经排着或在途时只记一笔,见 SessionInfo::loginRenewalRearm。
void NoteLoginSucceeded(SessionId sessionId, SessionInfo &session, SteadyTime now);

// 每秒一拍:给到期的连接发起续租,每拍至多 kMaxRequestsPerTick 个,在途不超过 kMaxInFlight。
// 没有到期项时只做一次堆顶比较。
void Tick(SteadyTime now);

// 下一次续租距应答到达的间隔(纯函数,单测直接调):login 给的秒数夹到 [kMinInterval, kMaxInterval],再按会话号提前至多五分之一,
// 把同一时刻登录的一批连接摊开(开服时成批登录的连接不会每个周期都在同一秒续租)。
std::chrono::seconds NextInterval(uint32_t renewAfterSeconds, SessionId sessionId);

// 进程级累计计数(gate loop 线程,单调不减),供汇总日志与单测读增量。
struct Counters
{
	uint64_t scheduled{0};  // 登录成功后排上续租的连接(同一连接停止后再次登录会再计一次)
	uint64_t requested{0};  // 发出的续租请求
	uint64_t notSent{0};    // 出口没发出(没有路由服 / login、缺通道、签名失败)
	uint64_t renewed{0};    // login 回已续期
	uint64_t gone{0};       // login 回没有可续的记录,这条连接停止续租
	uint64_t rearmed{0};    // 回「没有可续的」时发现在途期间又登录成功过,立刻再问一次
	uint64_t failed{0};     // 调用失败或应答不可解析
	uint64_t staleReply{0}; // 应答到达时连接已断开,或会话号已被新连接复用
	uint64_t dropped{0};    // 到期时连接已断开,或会话号已被新连接复用

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();

// 排着的续租数(含已断开连接尚未到期的登记)与在途请求数,只作观测与单测。
std::size_t PendingRenewals();
uint64_t InFlight();

// 清空本线程的全部状态(到期堆、在途计数、计数),不动注入的出口。只给单测用。
void ResetForTesting();

} // namespace gate_login_renewal
