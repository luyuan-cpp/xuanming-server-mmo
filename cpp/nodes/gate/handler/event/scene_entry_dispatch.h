#pragma once

#include <chrono>
#include <memory>
#include <string>

#include "muduo/net/Callbacks.h"
#include "type_define/type_define.h"

struct SessionInfo;
struct SceneRouteTarget;

// gate 侧 RoutePlayerEvent 进场转发的补发与收口(CPP-2;设计与残余见 cross-zone-scene-travel.md 的
// "CPP-2 gate 侧进场转发补发"一节)。
//
// 生成式 handler 的守护段(gate_event_handler.cpp 的 RoutePlayer / BindSession、
// rpc_replies/scene_response_handler.cpp 的 SceneNodeHandshake 应答)只放一行委托,逻辑集中在本手写文件,
// 重生成时不受影响(照 battle_binding_helper.h 的写法)。判定逻辑本身是 scene_route_helper.h 里的纯函数,
// 本文件只负责接注册表、连接、定时器与日志。
//
// 契约:
//   - 线程:全部入口只在 gate 主 EventLoop 线程调用(Kafka 回调 queueInLoop 到主 loop、RPC 应答与
//     TimerTaskComp 也在这个线程),内部状态是 thread_local,不加锁。
//   - 一次路由决策最多真正转发一次(kSent 即清欠账);只有本地确定没发出去才续期,有总预算与次数上限。
//   - 超限收口:推 kEnterSceneFailed(3023)→ 发 KickPlayer(reason=kEnterSceneFailed,即消息 34)
//     → shutdown → forceCloseWithDelay。ExitGame 与 login 断线租约仍由断线回调
//     (client_message_processor.cpp HandleConnectionDisconnection)统一处理,本文件不重复。
//   - 截止判断一律用单调时钟(调用方传入 now),muduo 定时器只负责唤醒。
namespace gate_scene_entry
{
// 与 pending_scene_entry_comp.h 的 SceneEntryClock 是同一个时钟(.cpp 里 static_assert 钉住)。
using Clock = std::chrono::steady_clock;

// 扫描周期 = 首次退避(gate_scene_route::kSceneEntryInitialBackoff);只负责唤醒,截止判断用单调时钟。
inline constexpr double kSweepIntervalSeconds = 0.25;
// 放弃时先 shutdown 让 tip 与 34 发出去,再留 1s 后强关:不配合四次挥手的客户端也不能把会话一直挂着。
inline constexpr double kGiveUpForceCloseDelaySeconds = 1.0;

// "本 gate 在这条出站连接上已与该 scene 握过手"的印章,挂在 SceneNodeService 注册表的节点实体上。
// 只存 weak_ptr(AGENTS.md §11.7:应用层不拥有连接)。按连接对象比对:重连后旧章自然失效,
// 不需要挂断线钩子;实体销毁时随之消失。
struct SceneLinkHandshakeComp
{
    std::weak_ptr<muduo::net::TcpConnection> conn;
};

// RoutePlayerEvent 到达:记下 lastSceneRoute,整体替换欠账(以最近一次路由决策为准,依据不是 epoch:
// 同落点重连不铸造 epoch),并立即尝试一次。调用前调用方已写好 playerId / homeZoneId / ownerEpoch。
void OnRouteDecision(SessionId sessionId, SessionInfo &session, const SceneRouteTarget &target, Clock::time_point now);

// BindSession 带来了登录类型(调用方已写入 session.pendingEnterGsType):已有欠账则立即尝试一次;
// 否则按 lastSceneRoute 建一笔有上限的欠账(还没收到路由时什么都不做,等路由自己到来)。
void OnLoginTypeBound(SessionId sessionId, SessionInfo &session, Clock::time_point now);

// scene 握手成功应答(conn 是应答所走的出站连接):核对它就是该节点 RpcClient 的当前连接后盖章,
// 并把以该节点为目标的欠账提前到下一轮扫描(不在应答处理器里同步转发)。
void OnSceneLinkHandshaken(const muduo::net::TcpConnectionPtr &conn, const std::string &nodeUuid, Clock::time_point now);

// 250ms 扫描定时器的回调:按需打 30s 汇总,对到期的欠账各尝试一次。
void RetryDueSceneEntries(Clock::time_point now);
}
