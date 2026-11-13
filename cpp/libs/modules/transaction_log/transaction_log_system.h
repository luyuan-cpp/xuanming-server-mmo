#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <string>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/currency/constants/currency.h"
#include "proto/common/rollback/transaction_log.pb.h"

// Builds TransactionLogEntry messages. Gameplay never waits on them: refusals are logged.
//
// Usage:
//   TransactionLogSystem::LogCurrencyAdd(player, kCurrencyGold, 100, balanceBefore, balanceAfter);
//   TransactionLogSystem::LogItemDestroy(player, itemUuid, configId, qty, corrId);
//
// 流水只有一条出路:随玩家存盘提交,和存档在同一个事务里落库(2026-11-11,审计发件箱,docs/design/player-audit-outbox.md)。
// 流水描述的是玩家存档里的改动。改动在内存里一发生就发 Kafka 的话,存盘最终没提交(scene 崩溃、实体被废黜、
// 存盘被确定性拒绝)时,流水里会留下一笔从未落库的收益或扣减:人工终结工具会凭它判「scene 已应用」(一物两得),
// GM 回收会按它多挂一笔玩家从没拿到的欠款;反过来提交之后才发 Kafka(11-05 的提交闸门),Kafka 不可用超过
// message.timeout.ms 或进程在发送前崩溃,已提交的改动就没有流水(断线重连 R13)。所以每条流水进它那名玩家的审计队列
// (AuditOutbox),随他的下一笔存盘交给 go/db,与存档在同一个 TiDB 事务里写进发件箱,再由 go/db 的转发循环投 Kafka。
// 于是「流水里有这条」等于「这笔改动已提交」,反过来也成立(Kafka 停多久都不丢)。
//
// C++ 不直接发审计 topic,没有第二条出路:
//   - 一条流水必须只涉及一名玩家(from_player、to_player 恰好填一个),才能随那名玩家的存盘提交。两名玩家之间的转移记成两条
//     (转出方一条、转入方一条,同一个 correlation_id),各随各的存盘;两边都填或都不填的流水拒收并打 ERROR。
//   - 没开审计发件箱的进程(没有存盘:battle、工具、没装配存盘状态机的单测)记流水同样拒收并打 ERROR。
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
    // 两名玩家之间转移物品没有单独的入口:转出方记 LogItemDestroy、转入方记 LogItemCreate,带同一个 correlationId(类注释)。

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

    // Record a fully custom entry:盖上 zone,进它那名玩家的审计队列(类注释);排队、放行与丢弃由 AuditOutbox 和存盘状态机处置。
    // tx_id 无效(号段没号)、不是恰好一名玩家、没开审计发件箱、单条超限时拒收,打 ERROR 并按原因计数(audit_outbox_stats)。
    // 有单一玩家时,不论收下还是被拒,判定之后都调一次 SetAssetChangedHook 装的钩子。
    static void RecordEntry(const TransactionLogEntry &entry);

    // 资产变动通知:每条流水只要指名了恰好一名玩家,RecordEntry 判定完(收下或被拒)就按这名玩家调一次。传空函数 = 不通知。
    // 用途:货币、物品的每次变动都由 CurrencySystem、BagService 自己记流水,玩法代码绕不开这里,所以 scene 在这一个收口处
    // 请求一次合并存盘(PlayerLifecycleSystem::RequestKeyProgressSave),资产变动约 1 秒内提交,不靠每个玩法各自记得触发
    // (docs/design/player-data-tidb-scaling-plan.md §4.5)。通知跟着「资产变了」走,不跟着「流水收下了」走:
    // 流水被拒(号段断供、单条超限)时货币、物品照样已经在内存里变了,同样要尽快存盘,否则崩溃时多丢的正好是资产。
    // 没有单一玩家的流水不通知:不知道该请谁存盘(scene 里也没有这样的调用方)。快照请求不走这里,要不要尽快提交由请求方决定。
    // 约束:钩子里只许登记「稍后存盘」,不许同步存盘或再记流水 —— 调用点在货币、背包的改动函数中间,改动还没做完。
    // 线程:钩子是线程局部的,与审计队列同一条线程;钩子里可以换掉或清掉钩子自己。
    using AssetChangedHook = std::function<void(uint64_t playerId)>;
    static void SetAssetChangedHook(AssetChangedHook hook);

private:
    // Get the player_id from an entity. Returns 0 if not resolvable.
    static uint64_t ResolvePlayerId(entt::entity player);

    // Generate a unique tx_id from the txlog id segment; kInvalidGuid when the segment
    // has no id in hand (RecordEntry then drops the entry, fail-closed).
    static uint64_t GenerateTxId();
};
