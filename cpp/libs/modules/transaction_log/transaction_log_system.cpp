#include "transaction_log_system.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>
#include <utility>
#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "modules/audit/audit_outbox.h"
#include "modules/audit/audit_outbox_stats.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node_config_manager.h"

namespace
{
    using audit_outbox_stats::DropReason;

    // 一条流水只能随一名玩家的存盘提交:返回那名玩家,两边都有玩家或都没有玩家时返回 0。
    // 两名玩家之间的转移要记成两条(转出方一条、转入方一条,同一个 correlation_id),各随各的存盘提交。
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

    // SetAssetChangedHook 装的钩子。线程局部,与审计队列同一条线程。
    TransactionLogSystem::AssetChangedHook &AssetChangedHookSlot()
    {
        thread_local TransactionLogSystem::AssetChangedHook hook;
        return hook;
    }

    // 收下或拒收一条流水。owner 是 SingleOwner 的结果。拒收都打 ERROR 并按原因计数(audit_outbox_stats);
    // 单条超限的那一种由 AuditOutbox 自己打、自己计。
    void QueueOrRefuse(const TransactionLogEntry &entry, uint64_t owner)
    {
        if (entry.tx_id() == kInvalidGuid || entry.tx_id() == 0)
        {
            // GenerateTxId 返回 kInvalidGuid 说明 txlog 号段没号(种类未启用、两段耗尽且
            // data_service 还没把续段送回来)。流水的价值全在 tx_id 唯一,写一条 0 号流水会与其它
            // 0 号流水混在一起,比不写更糟。fail-closed:这条不记、打日志;没有 snowflake 回退。
            audit_outbox_stats::IncDropped(DropReason::kNoId);
            LOG_ERROR << "TransactionLogSystem: refusing to record entry with invalid tx_id (txlog id segment not ready)"
                      << " tx_type=" << entry.tx_type() << " correlation_id=" << entry.correlation_id()
                      << " from_player=" << entry.from_player() << " to_player=" << entry.to_player()
                      << " " << tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Describe() << " (metric=txlog_refused_no_id)";
            return;
        }

        // 一条流水只能随一名玩家的存盘提交(类注释)。没有单一玩家的跟不了任何一次提交:拒收,不绕开存盘直接发。
        // 带上关联号:人工终结工具的核对清单要能按 correlation_id 查到「这条流水没记上」。
        if (owner == 0)
        {
            audit_outbox_stats::IncDropped(DropReason::kNoSingleOwner);
            LOG_ERROR << "TransactionLogSystem: refusing entry tx_id=" << entry.tx_id() << " tx_type=" << entry.tx_type()
                      << " correlation_id=" << entry.correlation_id() << " from_player=" << entry.from_player()
                      << " to_player=" << entry.to_player()
                      << ": an entry must name exactly one player so it can commit with that player's save"
                      << " (metric=txlog_refused_no_single_owner)";
            return;
        }
        // 没开审计发件箱的进程没有存盘,流水没有提交的地方(scene 在装配存盘状态机时打开)。
        if (!AuditOutbox::Enabled())
        {
            audit_outbox_stats::IncDropped(DropReason::kOutboxDisabled);
            LOG_ERROR << "TransactionLogSystem: audit outbox is not enabled in this process; refusing entry tx_id=" << entry.tx_id()
                      << " tx_type=" << entry.tx_type() << " correlation_id=" << entry.correlation_id() << " player=" << owner
                      << " (metric=txlog_refused_outbox_disabled)";
            return;
        }

        // 改动发生时的 zone 由这里统一盖章(不散到各个 Log* 里各写一遍),消费者不许回查 Router(合服后 home_zone 会变)。
        // 记的是改动发生时的 zone,不是提交时的。入参是 const 引用,盖章落在一份本地副本上。
        // 取 GameConfig.zone_id 而非 GetZoneId():两者同源(node.cpp 用它填 NodeInfo.zone_id),
        // 但 modules 工程的包含路径里没有 engine/core,引不到 network/node_utils.h。
        TransactionLogEntry stamped = entry;
        stamped.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

        // 进这名玩家的审计队列,随他的存盘在同一个 TiDB 事务里落进发件箱,再由 go/db 的转发循环投 Kafka
        // (audit_outbox.h,docs/design/player-audit-outbox.md)。这里被拒只剩单条超限一种,AuditOutbox 已打 ERROR、已计数。
        (void)AuditOutbox::EnqueueTransactionLog(owner, std::move(stamped));
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
// RecordEntry — 流水只有一条出路:进这名玩家的审计队列,随他的存盘提交
// ---------------------------------------------------------------------------

void TransactionLogSystem::RecordEntry(const TransactionLogEntry &entry)
{
    const uint64_t owner = SingleOwner(entry);
    QueueOrRefuse(entry, owner);

    // 货币、物品已经在内存里变了:不论这条流水收下还是被拒,都请这名玩家尽快存盘(头文件 SetAssetChangedHook)。
    // 放在判定之后:收下时被通知的一方此刻取快照就带得上这一条。拷一份再调:钩子里若换掉或清掉钩子,不能毁掉正在执行的函数对象。
    if (owner != 0)
    {
        if (const auto hook = AssetChangedHookSlot())
        {
            hook(owner);
        }
    }
}

void TransactionLogSystem::SetAssetChangedHook(AssetChangedHook hook)
{
    AssetChangedHookSlot() = std::move(hook);
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
