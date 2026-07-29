#include "transaction_log_system.h"

#include <chrono>
#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "thread_context/snow_flake_manager.h"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

uint64_t TransactionLogSystem::ResolvePlayerId(entt::entity player)
{
    const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
    return (guid != nullptr && *guid != kInvalidGuid) ? *guid : 0;
}

uint64_t TransactionLogSystem::GenerateTxId()
{
    return tlsSnowflakeManager.GenerateItemGuid();
}

static uint64_t NowUnixSeconds()
{
    return static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::seconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count());
}

// ---------------------------------------------------------------------------
// SendEntry — serialize and push to Kafka
// ---------------------------------------------------------------------------

void TransactionLogSystem::SendEntry(const TransactionLogEntry &entry)
{
    if (entry.tx_id() == kInvalidGuid || entry.tx_id() == 0)
    {
        // GenerateTxId 返回 kInvalidGuid 说明发号器被 fence(节点失去 node_id 身份)
        // 或该线程未初始化。流水的价值全在 tx_id 唯一,写一条 0 号流水会与其它
        // 0 号流水混在一起,比不写更糟。fail-closed。
        LOG_ERROR << "TransactionLogSystem: refusing to emit entry with invalid tx_id"
                  << " from_player=" << entry.from_player()
                  << " to_player=" << entry.to_player();
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

    auto err = KafkaProducer::Instance().send(kTransactionLogTopic, bytes, key);
    if (err != RdKafka::ERR_NO_ERROR)
    {
        LOG_ERROR << "TransactionLogSystem: Kafka send failed for tx_id="
                  << entry.tx_id() << " err=" << static_cast<int>(err);
    }
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
    TransactionType txType)
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
    SendEntry(entry);
}

void TransactionLogSystem::LogCurrencyDeduct(
    entt::entity player,
    CurrencyType type,
    uint64_t amount,
    uint64_t balanceBefore,
    uint64_t balanceAfter,
    TransactionType txType)
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
    SendEntry(entry);
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
    SendEntry(entry);
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
    SendEntry(entry);
}

void TransactionLogSystem::LogItemCreate(
    entt::entity player,
    uint64_t itemUuid,
    uint32_t configId,
    uint32_t quantity,
    TransactionType txType)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(txType);
    entry.set_to_player(ResolvePlayerId(player));
    entry.set_item_uuid(itemUuid);
    entry.set_item_config_id(configId);
    entry.set_item_quantity(quantity);
    SendEntry(entry);
}

void TransactionLogSystem::LogItemDestroy(
    entt::entity player,
    uint64_t itemUuid,
    uint32_t configId,
    uint32_t quantity)
{
    TransactionLogEntry entry;
    entry.set_tx_id(GenerateTxId());
    entry.set_timestamp(NowUnixSeconds());
    entry.set_tx_type(TX_ITEM_DESTROY);
    entry.set_from_player(ResolvePlayerId(player));
    entry.set_item_uuid(itemUuid);
    entry.set_item_config_id(configId);
    entry.set_item_quantity(quantity);
    SendEntry(entry);
}
