#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <string>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/currency/constants/currency.h"
#include "proto/common/rollback/transaction_log.pb.h"

// Kafka topic for transaction log entries —— 这里是**基名**,不是实际发送用的名字。
//
// 实际名字带 topic 世代后缀 `_g<N>`(modules/audit/audit_topic.h 的 AuditTopicName,0 = 第一代),世代号来自部署配置
// AuditTopicGeneration,与 go/data_service 的 Kafka.TopicGeneration、go/db 的 AuditOutbox.TopicGeneration 对齐;
// 不一致就是写进没人消费的 topic,流水静默丢失。**别把这个常量直接交给 KafkaProducer** —— 裸名字会被 broker 自动建成
// 1 分区。2026-11-11 起 scene 上的流水由 go/db 的审计发件箱转发(下面类注释),C++ 只直接发不进发件箱的那一小部分。
constexpr char kTransactionLogTopicBase[] = "transaction_log_topic";

// Builds TransactionLogEntry messages. Gameplay never waits on them: refusals and failures are logged.
//
// Usage:
//   TransactionLogSystem::LogCurrencyAdd(player, kCurrencyGold, 100, balanceBefore, balanceAfter);
//   TransactionLogSystem::LogItemTransfer(fromPlayer, toPlayer, itemUuid, configId, qty, TX_TRADE, corrId);
//
// 流水跟着存盘提交走,而且和存档在同一个事务里落库(2026-11-11,审计发件箱,docs/design/player-audit-outbox.md):
// 流水描述的是玩家存档里的改动。改动在内存里一发生就发 Kafka 的话,存盘最终没提交(scene 崩溃、实体被废黜、
// 存盘被确定性拒绝)时,流水里会留下一笔从未落库的收益或扣减:人工终结工具会凭它判「scene 已应用」(一物两得),
// GM 回收会按它多挂一笔玩家从没拿到的欠款;反过来提交之后才发 Kafka(11-05 的提交闸门),Kafka 不可用超过
// message.timeout.ms 或进程在发送前崩溃,已提交的改动就没有流水(断线重连 R13)。所以 scene 上开了审计发件箱
// (AuditOutbox::Enable)之后,只涉及一名玩家的流水进这名玩家的审计队列,随他的下一笔存盘交给 go/db,
// 与存档在同一个 TiDB 事务里写进发件箱,再由 go/db 的转发循环投 Kafka。于是「流水里有这条」等于「这笔改动已提交」,
// 反过来也成立(Kafka 停多久都不丢)。
// 不进发件箱的(没开发件箱的进程:battle、单测、工具;两边都有玩家或都没有玩家的流水)照旧直接发 Kafka,发送失败只打 ERROR。
//
// 线程:只在所属逻辑线程上调用;审计队列是线程局部的,scene 上就是 tlsEcs 所在的那条线程。
class TransactionLogSystem
{
public:
    // ── Currency operations ──────────────────────────────────────────────

    // correlationId 把这笔货币变动和产生它的那次业务操作对上(帮会 op_id、
    // 聚宝斋 listing_id / order_id)。0 = 无关联,与旧行为一致。
    // 没有它,通用资产通道出问题时只能按 (玩家, 时间, 金额) 猜是哪一次捐献。
    static void LogCurrencyAdd(
        entt::entity player,
        CurrencyType type,
        uint64_t amount,
        uint64_t balanceBefore,
        uint64_t balanceAfter,
        TransactionType txType = TX_CURRENCY_ADD,
        uint64_t correlationId = 0);

    static void LogCurrencyDeduct(
        entt::entity player,
        CurrencyType type,
        uint64_t amount,
        uint64_t balanceBefore,
        uint64_t balanceAfter,
        TransactionType txType = TX_CURRENCY_DEDUCT,
        uint64_t correlationId = 0);

    // Deferred clawback deduction event.
    static void LogClawbackDeduction(
        entt::entity player,
        CurrencyType type,
        uint64_t deductedAmount,
        uint64_t debtRemaining);

    // ── Item operations ──────────────────────────────────────────────────

    // Log an item being transferred between two players (or player ↔ system).
    static void LogItemTransfer(
        uint64_t fromPlayerId,
        uint64_t toPlayerId,
        uint64_t itemUuid,
        uint32_t configId,
        uint32_t quantity,
        TransactionType txType,
        uint64_t correlationId = 0);

    // Log an item being created (quest reward, GM grant, etc.).
    // correlationId / extra 可选:战斗掉落传 battle_id 与来源 JSON,
    // TX_ITEM_AWARD 的 proto 注释要求 extra 带来源(monster_id / drop_table),
    // 否则回滚重放无法判断这笔奖励是否仍然有效。
    static void LogItemCreate(
        entt::entity player,
        uint64_t itemUuid,
        uint32_t configId,
        uint32_t quantity,
        TransactionType txType,
        uint64_t correlationId = 0,
        const std::string &extra = {});

    // Log an item being destroyed / consumed.
    // correlationId / extra 同上:战斗内消耗的药水靠它关联回具体哪一场战斗。
    //
    // txType **排在最后**(而不是像 LogItemCreate 那样排在 correlationId 之前):
    // 既有调用点都按 (…, correlationId, extra) 的位置传参,插在中间会让它们静默
    // 错位 —— 那正是"加一个默认参数"最容易埋的雷。默认值 TX_ITEM_DESTROY 保持旧行为;
    // 聚宝斋按 guid 托管扣出传 TX_AUCTION_SELL:实例确实从玩家 blob 上消失了,
    // 但原因不是"消耗掉了",流水里必须分得开(docs/design/jubaozhai-market.md §6.2)。
    static void LogItemDestroy(
        entt::entity player,
        uint64_t itemUuid,
        uint32_t configId,
        uint32_t quantity,
        uint64_t correlationId = 0,
        const std::string &extra = {},
        TransactionType txType = TX_ITEM_DESTROY);

    // ── Generic / low-level ──────────────────────────────────────────────

    // Build and record a fully custom entry. 开了审计发件箱时,只涉及一名玩家的流水进这名玩家的审计队列(见类注释),
    // 排队、放行与丢弃由 AuditOutbox 和存盘状态机处置;其余直接发 Kafka。
    static void RecordEntry(const TransactionLogEntry &entry);

    // 单测用:替换直接发送(缺省发 Kafka)。只影响不进发件箱的流水。传空函数恢复缺省。
    using Publisher = std::function<void(const TransactionLogEntry &entry)>;
    static void SetPublisherForTest(Publisher publisher);

private:
    // 序列化并直接发 Kafka(或单测替换的发送函数)。只给不进发件箱的流水用,失败只打 ERROR。
    static void Publish(const TransactionLogEntry &entry);

    // Get the player_id from an entity. Returns 0 if not resolvable.
    static uint64_t ResolvePlayerId(entt::entity player);

    // Generate a unique tx_id from the txlog id segment; kInvalidGuid when the segment
    // has no id in hand (RecordEntry then drops the entry, fail-closed).
    static uint64_t GenerateTxId();
};
