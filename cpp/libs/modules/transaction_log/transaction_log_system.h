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
// 实际名字带 topic 世代后缀 `_g<N>`:分区数是 topic 的不可变契约(分区键 = player_id 的按玩家有序
// 全靠它),改分区数不许原地改,只能换一代 —— 新名字、新分区数、消费者(go/data_service)
// 同步切到同一代。世代号与 go/data_service/etc/data_service.yaml 的 Kafka.TopicGeneration 对齐;
// 两边不一致就是生产者写进没人消费的 topic,流水静默丢失。
//
// 2026-09-09:后缀不再是编译期常量,改由部署配置 AuditTopicGeneration 拼
// (modules/audit/audit_topic.h 的 AuditTopicName;0 = 第一代)。换代只改配置,不必改这里、
// 也不必重出镜像。**别把这个常量直接交给 KafkaProducer** —— 裸名字会被 broker 自动建成
// 1 分区,消费端的分区契约随即永久失配。
constexpr char kTransactionLogTopicBase[] = "transaction_log_topic";

// Builds TransactionLogEntry messages and sends them to Kafka for persistence by the Go DB service.
//
// Usage:
//   TransactionLogSystem::LogCurrencyAdd(player, kCurrencyGold, 100, balanceBefore, balanceAfter);
//   TransactionLogSystem::LogItemTransfer(fromPlayer, toPlayer, itemUuid, configId, qty, TX_TRADE, corrId);
//
// All methods are fire-and-forget: failures are logged but never block gameplay.
//
// 流水跟着存盘提交走(2026-11-05,docs/design/data-consistency-guarantee-map-20261020.md §6):
// 流水描述的是玩家存档里的改动。改动在内存里一发生就发 Kafka 的话,存盘最终没提交(scene 崩溃、实体被废黜、
// 存盘被确定性拒绝)时,流水里会留下一笔从未落库的收益或扣减:人工终结工具会凭它判「scene 已应用」(一物两得),
// GM 回收会按它多挂一笔玩家从没拿到的欠款。所以 scene 上开了提交闸门(EnableCommitGating)之后:
//   - 只涉及一名玩家的流水先进这名玩家的队列,带一个进程内单调的水位;
//   - 存盘状态机确认「这份快照里的资产改动已在库里」时,按取快照那一刻的水位放行(PublishThrough);
//   - 改动最终没落库(实体销毁、存盘落定之后还剩的,或这名玩家在本节点重新认领之前)整队丢掉(DiscardPending)。
// 于是「流水里有这条」就等于「这笔改动已提交」。反过来不成立:提交之后发送失败(只打 ERROR)、
// 提交成功但回包丢失时进程崩溃,都会让已提交的改动没有流水,判「从未应用」必须另有证据(已提交账本)。
// 没开闸门的进程(battle、单测、工具)立即发送,行为与以前相同。
//
// 线程:只在所属逻辑线程上调用。闸门状态是线程局部的,scene 上就是 tlsEcs 所在的那条线程。
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

    // Build and send a fully custom entry. 开了提交闸门时,只涉及一名玩家的流水先排队(见类注释)。
    static void RecordEntry(const TransactionLogEntry &entry);

    // ── 提交闸门(见类注释)──────────────────────────────────────────────

    // 开关提交闸门。scene 在装配存盘状态机时打开,和放行钩子(PublishThrough)一起接上:只开闸门不接钩子,
    // 流水永远出不去;只接钩子不开闸门,行为与以前相同。关掉时已排队的不动,仍由 PublishThrough / DiscardPending 处置。
    static void EnableCommitGating(bool enabled);

    // 此刻的水位:已经记下的流水,水位都不大于它。存盘取快照时调用并记在这份快照上。
    static uint64_t CommitMark();

    // 这名玩家水位不大于 mark 的流水已随存盘提交,按记录顺序发出。
    static void PublishThrough(uint64_t playerId, uint64_t mark);

    // 丢掉这名玩家还没放行的流水,返回条数:它们描述的改动没有落库。reason 只进日志。
    static std::size_t DiscardPending(uint64_t playerId, const char *reason);

    // 这名玩家还没放行的条数。日志与单测用。
    [[nodiscard]] static std::size_t PendingCount(uint64_t playerId);

    // 单测用:替换真正的发送(缺省发 Kafka)。传空函数恢复缺省。
    using Publisher = std::function<void(const TransactionLogEntry &entry)>;
    static void SetPublisherForTest(Publisher publisher);

private:
    // 序列化并发 Kafka(或单测替换的发送函数)。失败只打 ERROR。
    static void Publish(const TransactionLogEntry &entry);

    // Get the player_id from an entity. Returns 0 if not resolvable.
    static uint64_t ResolvePlayerId(entt::entity player);

    // Generate a unique tx_id from the txlog id segment; kInvalidGuid when the segment
    // has no id in hand (RecordEntry then drops the entry, fail-closed).
    static uint64_t GenerateTxId();
};
