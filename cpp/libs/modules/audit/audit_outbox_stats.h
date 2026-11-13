#pragma once

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>

// scene 一侧审计记录的进程级计数(docs/design/player-audit-outbox.md §5.6),与 currency_clawback_stats 同一套写法
// (relaxed atomic,进程启动以来的累计值)。由 scene 的 Prometheus 出口导出(cpp/libs/services/scene/player/system/player_save_metrics.cpp),
// 告警在 deploy/k8s/audit-outbox-alerts.yaml 的 scene.audit-outbox 组;两边的名字与取值由
// tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1 对照本文件核对。
//
// 线程:写在 scene 逻辑线程(审计队列是线程局部的);计数是进程级的原子量,出口读取时不加锁。
namespace audit_outbox_stats
{
    // 一条审计记录没能进队列、或者进了又被挤掉的原因。审计不挡游戏:每一种都只丢这一条记录,它描述的货币、物品改动照常生效、
    // 照常随存盘提交,所以每计一次就是「一笔改动没有流水 / 快照」。健康时全部恒 0;同一时刻日志里有一行带 metric= 标签的 ERROR,
    // 玩家号只在那一行(不进指标 label)。
    enum class DropReason : uint8_t
    {
        // 一名玩家排着的记录到了内存上限(AuditOutbox::kMaxRecordsPerPlayer / kMaxBytesPerPlayer),丢最旧的一条:
        // 这名玩家的存盘很久没提交了,而他还在不停地产生记录(metric=audit_outbox_overflow)。
        kOverflow,
        // 单条超过 AuditOutbox::kMaxRecordBytes,拒收(metric=audit_outbox_record_too_large)。调用方的问题,比如 extra 里塞了大段文本。
        kTooLarge,
        // 流水没有恰好一名玩家,跟不了任何一次存盘(metric=txlog_refused_no_single_owner)。
        kNoSingleOwner,
        // 这个进程没开审计发件箱(没有存盘的进程):流水或快照请求拒收(metric=txlog_refused_outbox_disabled / snapshot_refused_outbox_disabled)。
        kOutboxDisabled,
        // 号段没号:流水拿不到 tx_id、快照拿不到 snapshot_id(metric=txlog_refused_no_id / snapshot_refused_no_id)。
        kNoId,

        kCount
    };

    inline constexpr std::size_t kDropReasonCount = static_cast<std::size_t>(DropReason::kCount);

    // 指标 label reason 的取值,下标就是 DropReason。
    inline constexpr const char *kDropReasonNames[] = {
        "overflow",        // DropReason::kOverflow
        "too_large",       // DropReason::kTooLarge
        "no_single_owner", // DropReason::kNoSingleOwner
        "outbox_disabled", // DropReason::kOutboxDisabled
        "no_id",           // DropReason::kNoId
    };
    static_assert(sizeof(kDropReasonNames) / sizeof(kDropReasonNames[0]) == kDropReasonCount,
                  "kDropReasonNames 必须与 DropReason 一一对应:加了枚举项就得加名字");

    inline std::array<std::atomic<uint64_t>, kDropReasonCount> &DroppedCounters()
    {
        static std::array<std::atomic<uint64_t>, kDropReasonCount> g_counters{};
        return g_counters;
    }

    inline void IncDropped(DropReason reason)
    {
        DroppedCounters()[static_cast<std::size_t>(reason)].fetch_add(1, std::memory_order_relaxed);
    }

    inline uint64_t ReadDropped(DropReason reason)
    {
        return DroppedCounters()[static_cast<std::size_t>(reason)].load(std::memory_order_relaxed);
    }

    // 带了审计记录的存盘提交了,go/db 却没写发件箱(旧 go/db 不认识审计字段,player_store_client.cpp)。记录没丢:不放行,
    // 留在队列里随下一笔再交;持续出现说明发布次序错了(先 go/db、再 scene,设计 §10),队列涨到上限之后才开始按 kOverflow 丢
    // (metric=audit_outbox_store_too_old)。
    inline std::atomic<uint64_t> &StoreTooOldCounter()
    {
        static std::atomic<uint64_t> g_counter{0};
        return g_counter;
    }

    inline void IncStoreTooOld() { StoreTooOldCounter().fetch_add(1, std::memory_order_relaxed); }

    inline uint64_t ReadStoreTooOld() { return StoreTooOldCounter().load(std::memory_order_relaxed); }
} // namespace audit_outbox_stats
