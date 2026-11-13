#pragma once

// gate 排空时把本 gate 上已登录的连接迁到同 zone 的其他 gate(docs/design/gate-drain-session-migration.md §4.3)。
//
// 一条连接怎么被迁走:
//   login 每拍发迁移许可(GateDrainMigrationEvent)──OnLease──▶ 许可有效期内,Tick 每秒挑至多 rate 个合格会话
//   ──RequestSender──▶ 逐个向 login 要票(ClientPlayerLogin.RequestGateMigration;路由模式经路由服,直连模式直发 login)
//   ──应答 OK──▶ RedirectPusher 推 msg 124(与跨 zone 传送的 RedirectToGateEvent 同一个推送函数)
//   ──客户端照 RedirectFlow 换到新 gate;旧连接断开后走既有的断开回调:断线保留、租约内重连、回原场景。
//
// 另管一件与迁移同源的事:推过 124 的会话 kRedirectLinger 之后连接还在,由 Tick 关掉(滞留关闭,迁移与跨 zone 共用)。
// 客户端不跟随时,这个会话的路由请求已经一律回 1003,留着只会让玩家卡住。
//
// 本模块不碰路由 lane、不编码帧、不选节点:发请求与推 124 是装配层注入的两个函数(GateClientSessionHandler 构造时注入,
// 同 gate_lane::SetRouterSender 的做法),单测注入替身。状态是 gate loop 线程上的 thread_local;回调只捕获会话号、
// 会话代次与玩家号,不捕获对象,也不持有连接(AGENTS.md §11.7)。只能在 gate 的 EventLoop 线程上调用。
//
// 这里没有「重连 / 连上某类节点」时的业务逻辑:迁移只由 login 的许可驱动,到达之后完全是既有的 Login 与 EnterGame。

#include <chrono>
#include <cstdint>
#include <functional>

#include "engine/core/type_define/type_define.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/login/login.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "session/comp/session_info_comp.h"

namespace gate_drain_migration
{

using SteadyTime = std::chrono::steady_clock::time_point;

// Tick 的节拍(秒),GateRuntimeContext::drainMigrationTimer 用。速率按「每拍至多 rate 个」执行,所以必须是 1 秒。
inline constexpr double kTickIntervalSeconds = 1.0;
// 一个会话没要到票(无目标、调用失败、没发出)之后,隔这么久再试。
inline constexpr std::chrono::seconds kRetryBackoff{10};
// 推过 124 的会话连接还在多久就关掉。与 Java 版 game.gate.redirect-linger 的缺省值相同(GateLimits.DEFAULT_REDIRECT_LINGER)。
// 客户端换连接时先连新 gate、再关旧连接,旧连接通常在探测的 5 秒内就关了。
inline constexpr std::chrono::seconds kRedirectLinger{60};
// 停在选角界面的连接,转发过 EnterGame 之后这么久不迁。不短于 login EnterGame 后台链的总预算
// (go/login/internal/logic/clientplayerlogin/entergamelogic.go 的 enterGameChainBudget,5 分钟):那段时间里 login
// 可能还在给这条连接绑角色,迁走会让新连接上的 EnterGame 撞上「登录中」。改那边时同步改这里;
// go/login/internal/logic/clientplayerlogin/gate_drain_settle_crosslang_test.go 按下面这行的写法读本值对拍,改写法要同步那个用例。
inline constexpr std::chrono::minutes kEnterGameSettle{5};

// 一个会话这一拍按什么身份迁。票据绑不绑玩家由 login 按 SessionDetails 的 player_id 判,gate 只决定迁不迁。
enum class Candidate : uint8_t
{
	kSkip,       // 不迁:没验票、没登录成功、进场途中、选角界面但刚转发过 EnterGame、已推过 124、路由 lane 或场景 lane 有在途或排队的请求
	kInGame,     // 已进游戏:已绑角色、有场景路由、没有待补发的进场。票据绑定本人
	kRoleSelect, // 停在选角界面:登录过、没绑角色、距上次 EnterGame 已满 kEnterGameSettle。票据不绑玩家
};

// 纯判定,只看会话状态,不看本模块的调度状态(在途、退避)。Tick 选人与应答到达时的复核用同一份。
Candidate Classify(const SessionInfo &session, SteadyTime now);

// 要票的应答,两种出站模式统一成这一种。delivered = false:调用失败或应答不可解析(结果未知,按失败退避)。
struct RequestReply
{
	bool delivered{false};
	loginpb::GateMigrationResponse response;
};
// receivedAt = 应答到达时刻(单调时钟),由出口在回调点读取后传入:退避从这里起算,本模块自己不读时钟。
using ReplyCallback = std::function<void(const RequestReply &reply, SteadyTime receivedAt)>;

// 发 RequestGateMigration 的出口。返回 true = 已发出,done 之后恰好被调用一次(可能同步调用);
// false = 没发出,done 不会被调用。
using RequestSender = bool (*)(SessionId sessionId, const SessionInfo &session, ReplyCallback done);

// 推 msg 124 的结果。
enum class PushOutcome : uint8_t
{
	kPushed,           // 已写进连接,会话已标成已重定向
	kSessionGone,      // 会话已不在
	kIdentityMismatch, // 票据玩家与会话当前绑定的玩家不符(R13 身份栅栏),丢弃
	kConnectionGone,   // 会话还在、连接已释放
};
// 推 124 的出口:handler/gate_redirect_delivery.h 的 PushRedirect。ticketPlayerId 为 0 = 不核对玩家。
using RedirectPusher = PushOutcome (*)(SessionId sessionId, Guid ticketPlayerId, const RedirectToGateNotify &notify,
	SteadyTime now);

// 装配:gate 启动时注入一次,单测注入替身;传 nullptr 撤销。
void SetRequestSender(RequestSender sender);
void SetRedirectPusher(RedirectPusher pusher);

// login 发来的迁移许可。rate_per_second 或 lease_ms 为 0 立即停;否则开启或续期,有效期从收到时起按单调时钟计。
void OnLease(const contracts::kafka::GateDrainMigrationEvent &event, SteadyTime now);

// 每秒一拍:先做滞留关闭,再在许可有效时发起至多 rate 个要票请求(在途不超过 2 × rate)。
void Tick(SteadyTime now);

// 客户端 EnterGame 发给 login 的那一刻调用(client_session_handler.cpp ApplyLoginTransition):直连模式收包即发出,在收包点记;
// 路由模式在 lane 真正发出 EnterGame 时由发出钩子记,还排在 lane 里的 EnterGame 由 Classify 的「lane 有在途或排队」挡住。
void NoteEnterGameForwarded(SessionInfo &session, SteadyTime now);

// 会话刚被推了 124(PushRedirect 推送成功后调):记下时刻,登记滞留关闭。同一会话只登记第一次。
void NoteRedirected(SessionId sessionId, SessionInfo &session, SteadyTime now);

// 许可此刻是否有效(观测与单测)。
bool LeaseActive(SteadyTime now);

// 进程级累计计数(gate loop 线程,单调不减),供汇总日志与单测读增量。
struct Counters
{
	uint64_t leases{0};        // 收到的许可(含续期)
	uint64_t leasesStopped{0}; // 许可停止:到期、rate 或 lease 为 0、NOT_DRAINING、DISABLED
	uint64_t requested{0};     // 发出的要票请求
	uint64_t notSent{0};       // 出口没发出(没有路由服 / login、缺通道、签名失败)
	uint64_t offered{0};       // 推出的 124
	uint64_t notDraining{0};   // login 回 NOT_DRAINING
	uint64_t disabled{0};      // login 回 DISABLED
	uint64_t noTarget{0};      // login 回 NO_TARGET
	uint64_t failed{0};        // 调用失败、应答不可解析或 UNSPECIFIED
	uint64_t staleReply{0};    // 应答到达时会话已换、已推过 124 或状态已变
	uint64_t pushDropped{0};   // 推 124 时会话已不在、连接已释放或身份不符
	uint64_t lingerClosed{0};  // 滞留关闭的连接(迁移与跨 zone 共用)

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();

// 清空本线程的全部状态(许可、在途计数、滞留队列、计数),不动注入的出口。只给单测用。
void ResetForTesting();

} // namespace gate_drain_migration
