#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>

#include "engine/core/type_define/type_define.h"

// gate 侧"进场转发欠账"的纯数据(CPP-2;设计与残余见 cross-zone-scene-travel.md 的
// "CPP-2 gate 侧进场转发补发"一节)。逻辑在 nodes/gate/handler/event/scene_route_helper.h(纯函数,可单测)
// 与 scene_entry_dispatch.cpp(注册表、连接、定时器),本文件不放任何行为。
//
// 不变量:
//   I1 一次路由决策最多真正转发一次。只有"本地确定没发出去"的失败(节点找不到 / 没有 RpcClient /
//      TCP 未连上 / 当前连接未握手)才续期欠账;这些都在调用 CallRemoteMethod 之前判定。一旦 kSent
//      就清掉欠账,绝不重发 —— kSent 之后在路上丢失检测不到,是已登记的残余。
//   I2 欠账与路由快照都存在 SessionInfo 里,只由 gate 主 EventLoop 线程读写(Kafka 回调、RPC 应答、
//      定时器都投递到这个线程)。会话 erase 即随之取消;session_id 被复用时新 SessionInfo 上什么都没有。
//   I3 每次尝试都按业务 node_id 重新解析节点实体,不缓存实体号:节点被摘除后不会以同一实体回来,
//      同 uuid 重注册是"先销毁再创建"且不发 OnNodeRemoveEvent,缓存的实体号会悬空。
//   I5 会话的路由(sceneId + scene 节点指向)只在"转发成功或本来就不需要转发"时整体提交;失败窗口内
//      会话仍指向上一次成功提交的节点,断线 ExitGame 与客户端消息照旧发往那里。

using SceneEntryClock = std::chrono::steady_clock;

// 一次向 scene 转发 PlayerEnterGameNode 的结果。除 kSent 外都表示"本地确定没发出去"。
enum class SceneForwardResult : uint8_t
{
    // 已交给当前出站连接,且这条连接上已握过手。**不**代表 scene 已处理:字节进了发送缓冲后
    // 连接断开、或 scene 处理前崩溃,都检测不到。
    kSent,
    // 按 node_id 在 gate 的 scene 注册表里找不到节点实体(尚未发现 / 已被摘除),或实体已失效。
    kNodeNotFound,
    // 节点实体在,但还没有挂 RpcClient(node_connector 先建实体后连)。
    kNoRpcClient,
    // TCP 不在已连接态。RpcClient::CallRemoteMethod 此时只打一条 ERROR 就静默丢弃,必须在调用前拦下。
    kNotConnected,
    // TCP 已连上,但 scene 还没为本 gate 挂 RpcSession(握手在连上 0.5s 后才发)。此时发出的进场
    // scene 照常载入玩家,但 NotifyEnterScene 等下行推送找不到 RpcSession 会被丢掉。
    kHandshakePending,
    // 路由本身无效(scene_id 为 0)。不可重试,直接放弃。
    kInvalidRoute,
    kCount
};

// 平铺名表(AGENTS.md §11.2:不用 X-macro),下标即枚举值;日志里的 first_failure / last_failure 取自这里。
inline constexpr const char *kSceneForwardResultNames[] = {
    "sent",
    "node_not_found",
    "no_rpc_client",
    "not_connected",
    "handshake_pending",
    "invalid_route",
};
static_assert(std::size(kSceneForwardResultNames) == static_cast<std::size_t>(SceneForwardResult::kCount),
              "kSceneForwardResultNames 必须与 SceneForwardResult 一一对应");

inline const char *SceneForwardResultName(const SceneForwardResult result)
{
    const auto index = static_cast<std::size_t>(result);
    if (index >= std::size(kSceneForwardResultNames))
    {
        return "unknown";
    }
    return kSceneForwardResultNames[index];
}

// 一条路由决策本身(来自 RoutePlayerEvent)。
// playerId 为 0 表示事件没带玩家(未知,不参与玩家围栏);sceneId 为 0 表示"还没收到过路由"。
// node_id 从 1 起分配,但判断"有没有路由"只看 sceneId,不看 nodeId。
struct SceneRouteTarget
{
    Guid playerId{0};
    uint32_t nodeId{0};
    uint64_t sceneId{0};
};

// 一笔进场转发欠账:路由已到 gate,但还没交给 scene。
// 时间点一律是单调时钟(steady_clock):截止判断不能受墙钟跳变影响;muduo 定时器只负责唤醒。
struct PendingSceneEntry
{
    SceneRouteTarget target;
    uint32_t attempts{0};                    // 已尝试次数(含首次)
    SceneEntryClock::time_point routedAt{};  // 路由决策到达时刻;节点发现预算从这里起算
    SceneEntryClock::time_point deadline{};  // 总预算截止
    SceneEntryClock::time_point nextAttemptAt{};
    // firstFailure 只在第一次失败时写、lastFailure 每次都写:放弃日志要同时回答"一开始为什么没发出去"
    // 与"最后卡在哪一步"(例如先 not_connected、后节点被摘除)。kSent 在这里表示"还没失败过"。
    SceneForwardResult firstFailure{SceneForwardResult::kSent};
    SceneForwardResult lastFailure{SceneForwardResult::kSent};
};
