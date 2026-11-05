#pragma once

#include <chrono>

#include "type_define/type_define.h"

struct SessionInfo;
struct SceneRouteTarget;

// gate 侧 RoutePlayerEvent 进场转发的补发与收口(CPP-2;设计与残余见 cross-zone-scene-travel.md 的
// "CPP-2 gate 侧进场转发补发"一节)。
//
// 生成式 handler 的守护段(gate_event_handler.cpp 的 RoutePlayer / BindSession)只放一行委托,
// 逻辑集中在本手写文件,重生成时不受影响。判定逻辑本身是 scene_route_helper.h 里的纯函数,
// 本文件只负责接注册表、通信就绪能力、定时器与日志;欠账由独立扫描推进。
//
// 契约:
//   - 线程:全部入口只在 gate 主 EventLoop 线程调用(Kafka 回调 queueInLoop 到主 loop、RPC 应答与
//     TimerTaskComp 也在这个线程),内部状态是 thread_local,不加锁。
//   - 一次路由决策最多真正转发一次(kSent 即清欠账);只有本地确定没发出去才续期,有总预算与次数上限。
//   - 超限收口:推 kEnterSceneFailed(3023)→ 发 KickPlayer(reason=kEnterSceneFailed,即消息 34)
//     → shutdown → forceCloseWithDelay。ExitGame 与 login 断线租约仍由断线回调
//     (client_session_handler.cpp HandleConnectionDisconnection)统一处理,本文件不重复。
//   - 截止判断一律用单调时钟(调用方传入 now),muduo 定时器只负责唤醒。
namespace gate_scene_entry
{
// 与 pending_scene_entry_comp.h 的 SceneEntryClock 是同一个时钟(.cpp 里 static_assert 钉住)。
using Clock = std::chrono::steady_clock;

// 扫描周期 = 首次退避(gate_scene_route::kSceneEntryInitialBackoff);只负责唤醒,截止判断用单调时钟。
inline constexpr double kSweepIntervalSeconds = 0.25;
// 放弃时先 shutdown 让 tip 与 34 发出去,再留 1s 后强关:不配合四次挥手的客户端也不能把会话一直挂着。
inline constexpr double kGiveUpForceCloseDelaySeconds = 1.0;

// RoutePlayerEvent 到达:记下 lastSceneRoute,整体替换欠账(以最近一次路由决策为准,依据不是 epoch:
// 同落点重连不铸造 epoch),并立即尝试一次。调用前调用方已用 scene_route_helper.h 的 ApplyRouteIdentity
// 写好 playerId / homeZoneId / ownerEpoch / loginGeneration:转发同步发生,盖章从会话取值。
void OnRouteDecision(SessionId sessionId, SessionInfo &session, const SceneRouteTarget &target, Clock::time_point now);

// BindSession 带来了登录类型(调用方已用 ApplyLoginBinding 写入 playerId / loginGeneration / pendingEnterGsType,
// 代次必须先于本调用写入,见 scene-enter-session-fence.md §4):已有欠账则立即尝试一次;
// 否则按 lastSceneRoute 建一笔有上限的欠账(还没收到路由时什么都不做,等路由自己到来)。
void OnLoginTypeBound(SessionId sessionId, SessionInfo &session, Clock::time_point now);

// 客户端 LeaveGame(回到选角界面,TCP 不断)到达 gate、转发给 login 之前调用(docs/design/location-release-binding.md §3.5 R5):
// 先向会话绑定的 scene 代发 ExitGame(与断线同一封包),再用 gate_scene_route::ResetSceneRoute 清空会话的 scene 路由状态并撤销欠账。
// 之后同一连接上的进场只由新的路由事件驱动,不按 LeaveGame 之前的旧路由补发。会话没绑 scene 时只清状态。不会 erase 会话。
void OnClientLeaveGame(SessionId sessionId, SessionInfo &session);

// 客户端 EnterGame 到达 gate、转发给 login 之前调用(与 OnClientLeaveGame 同一个收包点):记下"LeaveGame 之后客户端已再次请求进场",
// 从此 login 才可能为本连接发出新一次登录的 Bind 与路由;在这之前到达的一律按 LeaveGame 之前那条登录链丢弃
// (gate_scene_route::NoteEnterGameRequested,location-release-binding.md §3.5,第二轮审计 r2-gate#1)。只改会话标志,不转发、不 erase。
void OnClientEnterGame(SessionId sessionId, SessionInfo &session);

// 250ms 扫描定时器的回调:按需打 30s 汇总,对到期的欠账各尝试一次。
void RetryDueSceneEntries(Clock::time_point now);
}
