#pragma once

// 未成年人到点断开(docs/design/real-name-anti-addiction.md §5.4)。
//
// 未成年人只能在可玩日的 20:00–21:00 玩。能不能进由 login 在绑定会话时判;绑定成功的 Login 回包带着当天的截止时刻
// (LoginResponse.anti_addiction.play_deadline_ms,Unix 毫秒;0 = 不限)。到点断开由 gate 自己做,因为连接在 gate 手里:
//   成功的 Login 回包 ──NoteLoginSucceeded──▶ 记下(或清掉)本连接的截止时刻
//   每秒一拍 ──Tick──▶ 离截止不到 kCloseLeadMs 的连接:推一条带原因的踢线、关连接,断开回调照常收尾
// 这样到点那一刻不依赖 login、Kafka 与 Redis 可用,一拍里关多少条都是本地操作(原先挂在 login 的续租上,每台 gate 每秒
// 只续 200 条,未成年人多时最后一批会晚几十秒)。提前 kCloseLeadMs 关:保证 21:00:00 之前连接已经断开,login 记下的
// 下线时刻也早于它。
//
// login 那边另有一道兜底:过了截止时刻还收到这条连接的续租,就经 Kafka 踢线并打 ERROR
// (go/login/internal/logic/clientplayerlogin/anti_addiction.go renewWatchedSession)。
//
// 状态是 gate loop 线程上的 thread_local;到期登记只记会话号、会话代次与截止时刻,到期时三者都对得上才关,
// 不持有连接(AGENTS.md §11.7)。只能在 gate 的 EventLoop 线程上调用。

#include <chrono>
#include <cstddef>
#include <cstdint>

#include "engine/core/type_define/type_define.h"
#include "session/comp/session_info_comp.h"

namespace gate_minor_deadline
{

// Tick 的节拍(秒),GateRuntimeContext::minorDeadlineTimer 用。
inline constexpr double kTickIntervalSeconds = 1.0;
// 提前这么久关连接。比一拍(1 秒)加踢线发出去的时间宽裕;服务器之间的时钟差在毫秒级(节点启动时核对过时钟)。
inline constexpr uint64_t kCloseLeadMs = 5000;
// 推完踢线后先只关写端,客户端这么久还不挥手就强关(同 scene_entry_dispatch.h kGiveUpForceCloseDelaySeconds)。
inline constexpr double kForceCloseDelaySeconds = 1.0;

// 关连接的出口:推带原因的踢线、关连接。装配层注入(GateClientSessionHandler 构造时),单测注入替身;传 nullptr 撤销。
using CloseFn = void (*)(SessionInfo &session);
void SetCloser(CloseFn closer);

// 本连接收到了一次成功的 Login 回包(与置 SessionInfo::loginSucceeded 同一处调用)。playDeadlineMs 取自回包,0 = 不限。
// 以最近一次成功的登录为准:同一条连接换成成年人的账号登录,截止时刻随之清掉。
void NoteLoginSucceeded(SessionId sessionId, SessionInfo &session, uint64_t playDeadlineMs);

// 每秒一拍:关掉离截止不到 kCloseLeadMs 的连接。时刻由调用方给(单测接缝);没有到期项时只做一次堆顶比较。
void Tick(uint64_t nowUnixMs);

// 墙钟的 Unix 毫秒。截止时刻是 login 按北京时间算出的绝对时刻,只能和墙钟比。
inline uint64_t UnixNowMs()
{
	return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
		std::chrono::system_clock::now().time_since_epoch()).count());
}

// 进程级累计计数(gate loop 线程,单调不减)。
struct Counters
{
	uint64_t armed{0};  // 记下截止时刻的登录
	uint64_t closed{0}; // 到点关掉的连接

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();

// 到期登记的条数(含已断开的连接还没到期的登记),只作观测。
std::size_t Pending();

// 只给单测用:清空登记、计数与出口。
void ResetForTesting();

} // namespace gate_minor_deadline
