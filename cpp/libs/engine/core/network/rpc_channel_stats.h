#pragma once

// 节点间 RPC0 的进程级计数(docs/design/muduo-rpc-channel.md §12)。
//
// C++ 节点没有 Prometheus 出口,口径与 exit_persist_stats / [lane] 汇总一致:relaxed 原子计数 + 每 30s 一行汇总,
// 只在计数变化时打印;压测复盘由 stress_summarize 解析这一行。字段名是与解析脚本之间的契约,改名要两边一起改
// (FormatRpcChannelStats 有单测锁定)。只用低基数维度,绝不出现 player_id / session_id。
//
// 逐条日志一律经 ShouldLogRpcEvent 采样(前 8 条逐条,之后每 1024 条一条),准确的总数以这里的计数为准。

#include <atomic>
#include <cstdint>
#include <string>

namespace muduo::net
{
class EventLoop;
}

struct RpcChannelStats
{
    // 客户端:每次 CallMethod 的结局,started == 各结局之和 + 在途数。
    std::atomic<uint64_t> started{0};
    std::atomic<uint64_t> ok{0};
    std::atomic<uint64_t> notSent{0};
    std::atomic<uint64_t> remoteRejected{0};
    std::atomic<uint64_t> remoteFailed{0};
    std::atomic<uint64_t> replyDropped{0};
    std::atomic<uint64_t> timeout{0};
    std::atomic<uint64_t> disconnected{0};
    std::atomic<uint64_t> channelClosed{0};
    std::atomic<uint64_t> invalidResponse{0};
    // 回包查不到在途调用(已超时 / 已断连失败 / 对端回了未知 id)。
    std::atomic<uint64_t> lateReply{0};
    // 回包 id == 0:对端是没有调用号的旧二进制,或对端违反「单向不回」的契约。切换完成后应恒为 0。
    std::atomic<uint64_t> legacyId0Reply{0};
    std::atomic<uint64_t> unknownType{0};
    std::atomic<uint64_t> onewaySent{0};
    std::atomic<uint64_t> onewayRejected{0};
    // channel 析构时还有在途调用、但已不在其 EventLoop 线程(loop 已析构):done 只能随容器销毁。关机收尾遗漏的信号。
    std::atomic<uint64_t> droppedWithoutLoop{0};
    std::atomic<uint64_t> maxInFlight{0};
    std::atomic<uint64_t> maxSweepLatenessMs{0};

    // 服务端。
    std::atomic<uint64_t> requestsDispatched{0};
    std::atomic<uint64_t> requestsRejected{0};   // 派发前拒绝(无效号 / 无服务 / 无方法 / 坏包)
    std::atomic<uint64_t> handlerFailed{0};      // handler 已运行并显式失败(SetFailed / RpcDoneCallback::Fail)
    std::atomic<uint64_t> adopted{0};
    std::atomic<uint64_t> replyRunTwice{0};
    std::atomic<uint64_t> replyChannelGone{0};
    std::atomic<uint64_t> replyAbandoned{0};

    // 链路心跳(docs/design/muduo-rpc-channel.md §21)。正常运行两项都应为 0。
    // 心跳收发次数故意不计:空闲链路上它每 5s 都在变,汇总行就会每 30s 打一次,失去「计数变化才打」的意义。
    std::atomic<uint64_t> linkIdleClosed{0};     // 读空闲满超时,判对端已死并 forceClose 的次数
    std::atomic<uint64_t> linkIdleDeferred{0};   // 读空闲已满,但检查迟到(本端 loop 卡过),推迟到下一次检查的次数
};

RpcChannelStats& GetRpcChannelStats();

// 当前计数的一行汇总(不含前缀),字段顺序固定。
std::string FormatRpcChannelStats(const RpcChannelStats& stats);

// 采样:第 n 次事件(n 从 1 起)是否逐条记录。前 8 条逐条,之后每 1024 条一条。
inline bool ShouldLogRpcEvent(uint64_t occurrence)
{
    return occurrence <= 8 || (occurrence & 1023) == 0;
}

inline void RaiseMax(std::atomic<uint64_t>& slot, uint64_t value)
{
    uint64_t current = slot.load(std::memory_order_relaxed);
    while (value > current && !slot.compare_exchange_weak(current, value, std::memory_order_relaxed))
    {
    }
}

// 每 30s 在 loop 上检查一次,计数有变化才打一行 `[RpcChannel] summary ...`。Node 构造时注册。
void RegisterRpcChannelStatsReporter(muduo::net::EventLoop& loop);
