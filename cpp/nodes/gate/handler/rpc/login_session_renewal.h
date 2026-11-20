#pragma once

// gate 这一侧「login 那条按连接记录」的整个生命周期(docs/design/gate-online-identity.md §7.3、§10.4):
// 连接活着时续租;连接关闭后把 Disconnect 送到 login 确认为止。
//
// 续租。login 的登录态、设备集成员与绑定栅栏都有寿命。寿命只是「Disconnect 送不到」(gate 进程没了)时的回收兜底,不该比活着的
// 连接先到期:登录态到期后,这条连接不再计入账号的设备数上限。所以连接活着期间由 gate 定期续租:
//   登录成功 ──NoteLoginSucceeded──▶ 下一拍向 login 发 ClientPlayerLogin.RenewSession
//                                    (只有 gate 合成;连接身份走签名的 SessionDetails,路由模式经路由服,直连模式直发 login)
//   ──应答 renewed──▶ 按 login 给的间隔(登录态寿命的三分之一)排下一次。寿命只在 login 配,gate 不另配
//   ──应答 not renewed──▶ login 已没有这条连接的记录(离开游戏、已按断线处理):停止,直到这条连接再次登录成功
//   ──调用失败 / 没发出──▶ kRetryBackoff 之后再试
// 连接断开后续租随之作废:到期项按会话号与会话代次核对,对不上就丢弃。续租失败不影响在线玩家:连续失败超过登录态寿命的后果
// 与没有续租时相同(这条连接不再计入设备数)。
//
// 断线通知(至少送达一次,docs/design/infra-reconnect-overview.md §1.2、§1.3)。Disconnect 是 login 关会话栅栏、清登录态、
// 给 player_locator 的会话挂断线租约的唯一入口,而会话键没有 TTL:丢一次,这个玩家就一直停在 ONLINE。所以:
//   连接关闭 ──NoteConnectionClosed──▶ 记下一条待送达的通知(断线当下的 SessionDetails),本拍预算有余量就当场发出
//   ──login 回成功(它自己的清理与 SetDisconnecting 都成了)──▶ 移除
//   ──调用失败 / login 回错误 / 没发出 / 结果未知──▶ DisconnectRetryBackoff 之后原样重发,不限次数
// 原样重发是安全的:login 与 player_locator 对重放幂等(会话栅栏、登录态按 gate 实例 + 会话代次认,断线租约重放不延长)。
// player_locator 只按会话号与 gate 实例认会话、不认代次,会话号回绕之后的残余见 gate-online-identity.md §10.4。
// 重试只有这一层,login 的 Disconnect 自己不重试。gate 进程没了,剩下的通知由 player_locator 的死 gate 清扫兜住
// (go/player_locator/internal/logic/gate_sweep.go)。
//
// 两类请求共用每拍预算 kMaxRequestsPerTick 与在途上限 kMaxInFlight。每拍先发断线通知、再发续租:断线通知每晚一秒,玩家就多挂
// 一秒 ONLINE;续租的间隔是寿命的三分之一,晚一些仍有余量。
//
// 本模块不碰路由 lane、不编码帧、不选节点:发请求是装配层注入的两个函数(GateClientSessionHandler 构造时注入,同
// gate_drain_migration::SetRequestSender 的做法),单测注入替身。状态是 gate loop 线程上的 thread_local;续租的回调只捕获会话号与
// 会话代次,断线通知的回调只捕获会话代次,都不捕获对象,也不持有连接(AGENTS.md §11.7)。只能在 gate 的 EventLoop 线程上调用。
//
// 这里没有「重连 / 连上某类节点」时的业务逻辑:续租只由本连接的登录成功与定时器驱动,断线通知只由本连接的关闭与定时器驱动。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <string>

#include "engine/core/type_define/type_define.h"
#include "metrics/prometheus_text.h"
#include "proto/login/login.pb.h"
#include "session/comp/session_info_comp.h"

namespace gate_login_renewal
{

using SteadyTime = std::chrono::steady_clock::time_point;

// Tick 的节拍(秒),GateRuntimeContext::loginRenewalTimer 用。
inline constexpr double kTickIntervalSeconds = 1.0;
// 每拍(两拍之间的一秒)至多发起的请求,续租与断线通知合计。login 故障恢复后所有连接同时到期时,按这个速率摊开,不在一拍里打出去。
// 5 万条连接的 gate 约 4 分钟补完续租,远小于「间隔是寿命三分之一」留下的余量。
inline constexpr uint32_t kMaxRequestsPerTick = 200;
// 在途请求的上限,续租与断线通知合计。应答迟迟不回(login 慢、路由服慢)时不再加压,等在途的回来或超时。
inline constexpr uint64_t kMaxInFlight = 2ull * kMaxRequestsPerTick;
// 一次续租没成(调用失败、没发出)之后,隔这么久再试。
inline constexpr std::chrono::seconds kRetryBackoff{30};
// login 给的间隔的合法范围,越界就夹回来:下限防止异常应答让一条连接每拍都续,上限防止间隔大到等于不续。
inline constexpr std::chrono::seconds kMinInterval{5};
inline constexpr std::chrono::seconds kMaxInterval{24 * 60 * 60};

// 断线通知没成之后的重发间隔:1 秒起每次翻倍、封顶 30 秒,再乘 [0.8, 1.2] 的抖动系数(DisconnectRetryBackoff)。
// 起点 1 秒:login / player_locator 重启、路由服换实例这类常见故障几秒内恢复,早一点重发,玩家少挂一会 ONLINE。
// 封顶 30 秒:故障持续时,每条通知每分钟只重发两次左右;合计再被每拍预算截住,login 恢复的那一刻不会集中打过去。
inline constexpr std::chrono::milliseconds kDisconnectRetryBase{1000};
inline constexpr std::chrono::milliseconds kDisconnectRetryCap{30000};
// 一条通知待送达超过这么久仍没被 login 确认,打一次 ERROR(带玩家号)。到这时玩家已经多挂了一分钟 ONLINE,
// 而 30 秒的断线租约还没开始算,值得人看一眼 login、player_locator 与路由服。
inline constexpr std::chrono::seconds kDisconnectOverdue{60};
// 待送达条数的上限 = 一个 gate 进程的会话号空间(SessionIdGenerator::kSeqMask,.cpp 里 static_assert 对齐)。理由:
//   - 它就是一台 gate 连接数的硬上限(connection_admission.h EffectiveMaxConnections),满载的 gate 在 login 长时间不可用时
//     全部连接断开,也装得下;
//   - 表里的通知比它多,说明会话号已经回绕过一圈,新连接与表里的旧通知同号的机会随之上来(player_locator 只按会话号与
//     gate 实例认会话,不认代次);
//   - 每条约 300 字节(序列化好的 SessionDetails 加调度状态),满表约 40 MB。
// 表满时新来的通知不入表,不静默丢:打带玩家号的 ERROR 并计数(gate_login_disconnect_total{result="dropped"}),
// 由玩家重登或 release-checklist 附录 F.B-1 的定向补救收口。
inline constexpr std::size_t kMaxPendingDisconnects = 131071;

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

// 每秒一拍:先重发到期的断线通知,再给到期的连接发起续租;两类合计每拍至多 kMaxRequestsPerTick 个(两拍之间当场发出的
// 断线通知也算在这一拍里),在途不超过 kMaxInFlight。没有到期项时只做两次堆顶比较。
void Tick(SteadyTime now);

// 下一次续租距应答到达的间隔(纯函数,单测直接调):login 给的秒数夹到 [kMinInterval, kMaxInterval],再按会话号提前至多五分之一,
// 把同一时刻登录的一批连接摊开(开服时成批登录的连接不会每个周期都在同一秒续租)。
std::chrono::seconds NextInterval(uint32_t renewAfterSeconds, SessionId sessionId);

// ---- 断线通知 ----

// 一条断线通知:断线当下这条连接的身份。sessionDetailsBin 是序列化好的 SessionDetails(带会话代次),装配层用与客户端请求
// 同一个构造点生成;重发时原样再签再发,不随之后发生的任何事改变。
struct DisconnectNotice
{
	SessionId sessionId{0};
	uint64_t incarnation{0};
	Guid playerId{0}; // 只进日志;0 = 还没绑角色
	std::string sessionDetailsBin;
};

// Disconnect 的应答,两种出站模式统一成这一种。delivered = true 才算 login 确认(它自己的清理与 SetDisconnecting 都成了);
// false:调用失败、login 回了错误(路由模式下是带 tip 的信封)或应答不可解析,一律当结果未知,退避之后原样重发。
struct DisconnectReply
{
	bool delivered{false};
	loginpb::LoginEmptyResponse response;
};
// receivedAt 的含义同 ReplyCallback:重发的退避从这里起算。
using DisconnectReplyCallback = std::function<void(const DisconnectReply &reply, SteadyTime receivedAt)>;

// 发 Disconnect 的出口,契约同 RequestSender:返回 true = 已发出,done 之后恰好被调用一次(可能同步调用);
// false = 没发出(直连模式挑不到 login、路由模式挑不到路由服或缺通道、签名失败),done 不会被调用。
using DisconnectSender = bool (*)(const DisconnectNotice &notice, DisconnectReplyCallback done);

// 装配:gate 启动时注入一次,单测注入替身;传 nullptr 撤销(撤销期间通知照样入表,装上之后照常发)。
void SetDisconnectSender(DisconnectSender sender);

// 连接关闭(断开回调,会话从会话表删掉之前调;本模块之后不再读会话)。把这条连接转成一条待送达的 Disconnect:
// 本拍预算与在途上限还有余量就当场发出,否则排到下一拍。同一会话代次只记一次。表满(kMaxPendingDisconnects)时不入表,
// 打 ERROR 并计数。
void NoteConnectionClosed(DisconnectNotice notice, SteadyTime now);

// 第 attempts 次(从 1 起)发送没成之后到下一次重发的间隔(纯函数,单测直接调):kDisconnectRetryBase 每次翻倍、
// 封顶 kDisconnectRetryCap,再乘 [0.8, 1.2] 的系数。系数由会话代次与次数算出,确定、可复算,同一时刻失败的一批通知各不相同。
std::chrono::milliseconds DisconnectRetryBackoff(uint32_t attempts, uint64_t incarnation);

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

	uint64_t disconnectQueued{0};    // 连接关闭时记下的断线通知
	uint64_t disconnectSent{0};      // 发出的 Disconnect 请求,含重发
	uint64_t disconnectRetried{0};   // 其中的重发
	uint64_t disconnectDelivered{0}; // login 确认、移出表的通知
	uint64_t disconnectFailed{0};    // 调用失败、login 回错误或应答不可解析
	uint64_t disconnectNotSent{0};   // 出口没发出
	uint64_t disconnectOverdue{0};   // 待送达超过 kDisconnectOverdue 的通知,每条至多计一次
	uint64_t disconnectDropped{0};   // 表满、没入表的通知

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();

// 排着的续租数(含已断开连接尚未到期的登记)、待送达的断线通知数(含在途)与在途请求数(两类合计),只作观测与单测。
std::size_t PendingRenewals();
std::size_t PendingDisconnects();
uint64_t InFlight();

// 把断线通知的计数写成 Prometheus 指标(metrics/metrics_registry.h 的采集回调):
//   gate_login_disconnect_pending          待送达(含在途)的断线通知条数;
//   gate_login_disconnect_total{result}    queued / sent / retried / delivered / failed / not_sent / overdue / dropped。
// 标签只有 result 一个封闭集合,不带玩家号与会话号(AGENTS.md §9);定位具体连接靠 [LoginDisconnect] 日志。
// 在 /metrics 出口所在的 gate 主 loop 上执行,读的就是本线程的计数。
void CollectMetrics(prometheus_text::Writer &writer);
// gate 启动时调一次,把 CollectMetrics 登记进进程的指标注册表;重复登记只记一行 ERROR。
void RegisterMetrics();

// 清空本线程的全部状态(到期堆、待送达表、本拍预算、在途计数、计数),不动注入的出口。只给单测用。
void ResetForTesting();

} // namespace gate_login_renewal
