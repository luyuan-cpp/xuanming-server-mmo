#include "transaction_log_system.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>
#include <utility>
#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "modules/audit/audit_outbox.h"
#include "modules/audit/audit_topic.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node_config_manager.h"

namespace
{
    // 单测替换的发送函数(SetPublisherForTest);空 = 发 Kafka。线程局部,与审计发件箱同一个线程模型。
    TransactionLogSystem::Publisher &TestPublisher()
    {
        thread_local TransactionLogSystem::Publisher publisher;
        return publisher;
    }

    // 只涉及一名玩家的流水才进审计发件箱:那名玩家的存盘提交了,它才算数。两边都有玩家的(LogItemTransfer,目前没有调用方)
    // 跟不了某一次提交,照旧直接发;两边都没有玩家的也直接发。
    uint64_t SingleOwner(const TransactionLogEntry &entry)
    {
        const uint64_t from = entry.from_player();
        const uint64_t to = entry.to_player();
        if ((from == 0) == (to == 0))
        {
            return 0;
        }
        return from != 0 ? from : to;
    }
} // namespace

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

uint64_t TransactionLogSystem::ResolvePlayerId(entt::entity player)
{
    const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    return (guid != nullptr && guid->value != kInvalidGuid) ? guid->value : 0;
}

// tx_id 走 txlog 种类的号段(docs/design/node-id-overhaul-plan-20260908.md §7.5 第 1 条):它只是
// Kafka / transaction_log 表的去重键与主键,不需要时间序 —— 时间在 timestamp 列里。
// 拿不到号(种类未启用 / 两段耗尽且续段未到)返回 kInvalidGuid,RecordEntry 据此 fail-closed。
uint64_t TransactionLogSystem::GenerateTxId()
{
    Guid txId = kInvalidGuid;
    if (tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).TryNext(txId))
    {
        return txId;
    }
    return kInvalidGuid;
}

static uint64_t NowUnixSeconds()
{
    return static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::seconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count());
}

// ---------------------------------------------------------------------------
// RecordEntry — 开了审计发件箱时随玩家存盘提交,否则直接发 Kafka
// ---------------------------------------------------------------------------

void TransactionLogSystem::RecordEntry(const TransactionLogEntry &entry)
{
    if (entry.tx_id() == kInvalidGuid || entry.tx_id() == 0)
    {
        // GenerateTxId 返回 kInvalidGuid 说明 txlog 号段没号(种类未启用、两段耗尽且
        // data_service 还没把续段送回来)。流水的价值全在 tx_id 唯一,写一条 0 号流水会与其它
        // 0 号流水混在一起,比不写更糟。fail-closed:这条不发、记日志;没有 snowflake 回退。
        LOG_ERROR << "TransactionLogSystem: refusing to emit entry with invalid tx_id (txlog id segment not ready)"
                  << " from_player=" << entry.from_player()
                  << " to_player=" << entry.to_player()
                  << " " << tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Describe();
        return;
    }

    // 捕获时刻的 zone 由生产者统一在这里盖章(不散到六个 Log* 里各写一遍),
    // 消费者不许回查 Router(合服后 home_zone 会变)。入参是 const 引用,盖章落在
    // 一份本地副本上;流水消息很小,复制成本远低于后面的 Kafka 发送。
    // 取 GameConfig.zone_id 而非 GetZoneId():两者同源(node.cpp 用它填 NodeInfo.zone_id),
    // 但 modules 工程的包含路径里没有 engine/core,引不到 network/node_utils.h。
    // 进发件箱的流水也在这里盖章:记的是改动发生时的 zone,不是提交时的。
    TransactionLogEntry stamped = entry;
    stamped.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

    // 开了审计发件箱(scene)时,只涉及一名玩家的流水进这名玩家的审计队列,随他的存盘在同一个 TiDB 事务里落进发件箱,
    // 再由 go/db 的转发循环投 Kafka(audit_outbox.h,docs/design/player-audit-outbox.md)。
    // 拒收只剩单条超限一种(已打 ERROR):宁可流水缺一条,也不绕开存盘直接发一条可能没落库的。
    if (const uint64_t owner = SingleOwner(stamped); owner != 0 && AuditOutbox::Enabled())
    {
        (void)AuditOutbox::EnqueueTransactionLog(owner, std::move(stamped));
        return;
    }
    Publish(stamped);
}

void TransactionLogSystem::Publish(const TransactionLogEntry &entry)
{
    if (const auto &publisher = TestPublisher())
    {
        publisher(entry);
        return;
    }

    std::string bytes;
    if (!entry.SerializeToString(&bytes))
    {
        LOG_ERROR << "TransactionLogSystem: failed to serialize entry tx_id=" << entry.tx_id();
        return;
    }

    // Use from_player as the Kafka partition key so all entries for a player
    // land in the same partition (preserves ordering per player).
    const std::string key = std::to_string(
        entry.from_player() != 0 ? entry.from_player() : entry.to_player());

    // 有效 topic 名 = 基名 + 部署配置里的世代后缀(audit::AuditTopicName);别用基名直发。
    auto err = KafkaProducer::Instance().send(audit::AuditTopicName(kTransactionLogTopicBase), bytes, key);
    if (err != RdKafka::ERR_NO_ERROR)
    {
        // 带上关联号和玩家号:人工终结工具的核对清单要能按 correlation_id 查到「这条流水没发出去」。
        // 走到这里的只有不进发件箱的流水(没开发件箱的进程、没有单一玩家的流水),发不出去就没了(设计 §12)。
        LOG_ERROR << "TransactionLogSystem: Kafka send failed for tx_id=" << entry.tx_id()
                  << " correlation_id=" << entry.correlation_id() << " tx_type=" << entry.tx_type()
                  << " from_player=" << entry.from_player() << " to_player=" << entry.to_player()
                  << " err=" << static_cast<int>(err) << " (metric=txlog_direct_send_failed)";
    }
}

void TransactionLogSystem::SetPublisherForTest(Publisher publisher)
{
    TestPublisher() = std::move(publisher);
}

// ---------------------------------------------------------------------------
// Currency operations
// ---------------------------------------------------------------------------

void TransactionLogSystem::LogCurrencyAdd(
    entt::entity player,
    CurrencyType type,
    uint64_t amount,
    uint64_t balanceBefore,
    uint64_t balanceAfter,
    TransactionType txType,
    uint64_t correlationId)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_to_player(ResolvePlayerId(player));
    entry.set_currency_type(static_cast<uint32_t>(type));
    entry.set_currency_delta(static_cast<int64_t>(amount));
    entry.set_balance_before(balanceBefore);
    entry.set_balance_after(balanceAfter);
    entry.set_correlation_id(correlationId);
    RecordEntry(entry);
}

void TransactionLogSystem::LogCurrencyDeduct(
    entt::entity player,
    CurrencyType type,
    uint64_t amount,
    uint64_t balanceBefore,
    uint64_t balanceAfter,
    TransactionType txType,
    uint64_t correlationId)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_from_player(ResolvePlayerId(player));
    entry.set_currency_type(static_cast<uint32_t>(type));
    entry.set_currency_delta(-static_cast<int64_t>(amount));
    entry.set_balance_before(balanceBefore);
    entry.set_balance_after(balanceAfter);
    entry.set_correlation_id(correlationId);
    RecordEntry(entry);
}

void TransactionLogSystem::LogClawbackDeduction(
    entt::entity player,
    CurrencyType type,
    uint64_t deductedAmount,
    uint64_t debtRemaining)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(TX_DEFERRED_CLAWBACK);
    entry.set_from_player(ResolvePlayerId(player));
    entry.set_currency_type(static_cast<uint32_t>(type));
    entry.set_currency_delta(-static_cast<int64_t>(deductedAmount));
    entry.set_extra("{\"debt_remaining\":" + std::to_string(debtRemaining) + "}");
    RecordEntry(entry);
}

// ---------------------------------------------------------------------------
// Item operations
// ---------------------------------------------------------------------------

void TransactionLogSystem::LogItemTransfer(
    uint64_t fromPlayerId,
    uint64_t toPlayerId,
    uint64_t itemUuid,
    uint32_t configId,
    uint32_t quantity,
    TransactionType txType,
    uint64_t correlationId)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_from_player(fromPlayerId);
    entry.set_to_player(toPlayerId);
    entry.set_item_uuid(itemUuid);
    entry.set_item_config_id(configId);
    entry.set_item_quantity(quantity);
    if (correlationId != 0)
    {
        entry.set_correlation_id(correlationId);
    }
    RecordEntry(entry);
}

void TransactionLogSystem::LogItemCreate(
    entt::entity player,
    uint64_t itemUuid,
    uint32_t configId,
    uint32_t quantity,
    TransactionType txType,
    uint64_t correlationId,
    const std::string &extra)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_to_player(ResolvePlayerId(player));
    entry.set_item_uuid(itemUuid);
    entry.set_item_config_id(configId);
    entry.set_item_quantity(quantity);
    if (correlationId != 0)
    {
        entry.set_correlation_id(correlationId);
    }
    if (!extra.empty())
    {
        entry.set_extra(extra);
    }
    RecordEntry(entry);
}

void TransactionLogSystem::LogItemDestroy(
    entt::entity player,
    uint64_t itemUuid,
    uint32_t configId,
    uint32_t quantity,
    uint64_t correlationId,
    const std::string &extra,
    TransactionType txType)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_from_player(ResolvePlayerId(player));
    entry.set_item_uuid(itemUuid);
    entry.set_item_config_id(configId);
    entry.set_item_quantity(quantity);
    if (correlationId != 0)
    {
        entry.set_correlation_id(correlationId);
    }
    if (!extra.empty())
    {
        entry.set_extra(extra);
    }
    RecordEntry(entry);
}
