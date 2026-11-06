#include "transaction_log_system.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>
#include <deque>
#include <unordered_map>
#include <utility>
#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "modules/audit/audit_topic.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node_config_manager.h"

// ---------------------------------------------------------------------------
// 提交闸门的状态(见头文件类注释)
// ---------------------------------------------------------------------------

namespace
{
    // 一条还没放行的流水。mark 是进程内单调的水位:存盘快照记的是取快照那一刻的水位,
    // 水位不大于它的流水描述的改动都在那份快照里。
    struct PendingEntry
    {
        uint64_t mark{0};
        TransactionLogEntry entry;
    };

    struct CommitGate
    {
        bool enabled{false};
        // 全进程(本线程)一条数轴,不按玩家分:实体销毁重建之后水位也不会回头,旧快照的水位盖不到新流水。
        uint64_t lastMark{0};
        std::unordered_map<uint64_t, std::deque<PendingEntry>> players;
        TransactionLogSystem::Publisher publisher;
        uint64_t overflowDropped{0};
    };

    CommitGate &Gate()
    {
        thread_local CommitGate gate;
        return gate;
    }

    // 一名玩家最多排这么多条。正常一个定时存盘周期(60s)里远到不了;到了说明这名玩家的存盘很久没有提交
    // (go/db 不通,或被确定性拒绝)。这时丢最旧的一条并打 ERROR:宁可流水缺一条,也不发一条没落库的。
    constexpr std::size_t kMaxPendingPerPlayer = 4096;
    // 溢出一旦开始,这名玩家之后每记一条就要丢一条;第一条和之后每这么多条打一次 ERROR,不刷屏。
    constexpr uint64_t kOverflowLogEveryN = 1000;

    // 只涉及一名玩家的流水才排队:那名玩家的存盘提交了,它才算数。两边都有玩家的(LogItemTransfer,目前没有调用方)
    // 跟不了某一次提交,照旧立即发;两边都没有玩家的也立即发。
    uint64_t GatedOwner(const TransactionLogEntry &entry)
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
// 拿不到号(种类未启用 / 两段耗尽且续段未到)返回 kInvalidGuid,SendEntry 据此 fail-closed。
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
// SendEntry — 开了提交闸门时先按玩家排队,否则立即发 Kafka
// ---------------------------------------------------------------------------

void TransactionLogSystem::SendEntry(const TransactionLogEntry &entry)
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
    // 排队的流水也在这里盖章:记的是改动发生时的 zone,不是放行时的。
    TransactionLogEntry stamped = entry;
    stamped.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

    auto &gate = Gate();
    if (const uint64_t owner = GatedOwner(stamped); gate.enabled && owner != 0)
    {
        auto &queue = gate.players[owner];
        if (queue.size() >= kMaxPendingPerPlayer)
        {
            if (gate.overflowDropped++ % kOverflowLogEveryN == 0)
            {
                const auto &oldest = queue.front().entry;
                LOG_ERROR << "TransactionLogSystem: player " << owner << " has " << queue.size()
                          << " transaction log entries waiting for a save commit; dropping the oldest tx_id=" << oldest.tx_id()
                          << " tx_type=" << oldest.tx_type() << " correlation_id=" << oldest.correlation_id()
                          << " instead of emitting an entry whose change may never be committed"
                          << " (dropped_total=" << gate.overflowDropped << ", metric=txlog_pending_overflow)";
            }
            queue.pop_front();
        }
        queue.push_back(PendingEntry{++gate.lastMark, std::move(stamped)});
        return;
    }
    Publish(stamped);
}

void TransactionLogSystem::Publish(const TransactionLogEntry &entry)
{
    if (const auto &publisher = Gate().publisher)
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
        LOG_ERROR << "TransactionLogSystem: Kafka send failed for tx_id=" << entry.tx_id()
                  << " correlation_id=" << entry.correlation_id() << " tx_type=" << entry.tx_type()
                  << " from_player=" << entry.from_player() << " to_player=" << entry.to_player()
                  << " err=" << static_cast<int>(err);
    }
}

// ---------------------------------------------------------------------------
// 提交闸门
// ---------------------------------------------------------------------------

void TransactionLogSystem::EnableCommitGating(bool enabled)
{
    Gate().enabled = enabled;
}

uint64_t TransactionLogSystem::CommitMark()
{
    return Gate().lastMark;
}

void TransactionLogSystem::PublishThrough(uint64_t playerId, uint64_t mark)
{
    auto &gate = Gate();
    const auto it = gate.players.find(playerId);
    if (it == gate.players.end())
    {
        return;
    }
    // 队列按水位递增(入队时水位单调),从头放行到第一条比 mark 大的为止。
    auto &queue = it->second;
    while (!queue.empty() && queue.front().mark <= mark)
    {
        Publish(queue.front().entry);
        queue.pop_front();
    }
    if (queue.empty())
    {
        gate.players.erase(it);
    }
}

std::size_t TransactionLogSystem::DiscardPending(uint64_t playerId, const char *reason)
{
    auto &gate = Gate();
    const auto it = gate.players.find(playerId);
    if (it == gate.players.end())
    {
        return 0;
    }
    const std::size_t count = it->second.size();
    gate.players.erase(it);
    LOG_WARN << "TransactionLogSystem: dropping " << count << " transaction log entr" << (count == 1 ? "y" : "ies")
             << " of player " << playerId << " whose changes were never committed (" << (reason != nullptr ? reason : "")
             << ", metric=txlog_discarded_uncommitted)";
    return count;
}

std::size_t TransactionLogSystem::PendingCount(uint64_t playerId)
{
    const auto &players = Gate().players;
    const auto it = players.find(playerId);
    return it == players.end() ? 0 : it->second.size();
}

void TransactionLogSystem::SetPublisherForTest(Publisher publisher)
{
    Gate().publisher = std::move(publisher);
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
    SendEntry(entry);
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
    SendEntry(entry);
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
    SendEntry(entry);
}
