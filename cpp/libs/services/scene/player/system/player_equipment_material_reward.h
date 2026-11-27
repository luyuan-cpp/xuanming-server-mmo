#pragma once

#include <cstdint>
#include <functional>
#include "engine/core/type_define/type_define.h"
#include "entt/src/entt/entity/entity.hpp"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/scene/player_bag.pb.h"

namespace data_service {
class SyncAccountStage1TradableRequest;
class SyncAccountStage1TradableResponse;
}

// scene EventLoop 单写。额度只来自收件箱中首次应用的正式战斗结算，与其永久水位同快照落盘。
// 两阶段总账（合入 A + 经济总纲 account-level 60 + 月卡资格）：
//   - 第一阶段（lifetime_earned < 660）：无每日/每周刷新，永久 660 份总池（首套 600 + 60 容错）；
//     其中胜局每满 10 份基础额包含至多 1 份可交易，跨角色/删角共享单账号终身 60 份硬上限（权威落在 data_service account_directory），余量全绑定。
//   - 第二阶段（lifetime_earned >= 660）：自动切换为每周 +150 份的 4 周滚存蓄水池（UTC+8 周一 05:00 补充，未用额度滚存封顶 600 份，防回拨），
//     零氪全绑定自用；持有有效「仙途轻享卡」（afk_pass_expires_at > nowSec）时每场有效胜局将其中 1 份解锁为市场可交易产出（不增总额度/战力）。
// 领取通过严格背包事务同时扣额度、记回执，可交易与绑定分堆入包；revision 只随成功领取增加，入账不改变领取水位。
// 生产环境领取含阶段一可交易份数的批次前，须等待 data_service 确认账号级 60 上限（fail-closed）；APPLIED 仍须 durable 才能向玩家展示成功。
class PlayerEquipmentMaterialRewardSystem {
public:
    static constexpr uint32_t kDailyLimit = 20;            // 兼容字段与单批领取上限
    static constexpr uint32_t kMaxClaimBatchUnits = 20;
    static constexpr uint32_t kMinClaimStepUnits = 5;
    static constexpr uint32_t kBattleUnits = 10;           // 单局基础份数
    static constexpr uint32_t kBonusUnits = 5;             // 困难 PVE / PVP 胜局额外绑定份数
    static constexpr uint32_t kLifetimeLimit = 660;        // 第一阶段首套永久总池
    static constexpr uint32_t kTradableLifetimeLimit = 60; // 第一阶段账号级跨角色终身可交易硬上限
    static constexpr uint32_t kWeeklyLimit = 150;          // 第二阶段每周补充额
    static constexpr uint32_t kWeeklyRolloverMaxWeeks = 4; // 第二阶段最多滚存 4 周
    static constexpr uint32_t kWeeklyRolloverMaxPool = kWeeklyLimit * kWeeklyRolloverMaxWeeks; // 600 份蓄水池封顶
    static constexpr uint32_t kPvpMinRoundsForLoss = 3;
    static constexpr uint32_t kPvpMinManualRoundsForLoss = 2;
    static constexpr uint32_t kCraftingMaterial = 3001;
    static constexpr uint32_t kEnhancementStone = 3002;

    struct BattleCreditDelta {
        uint32_t totalUnitsAdded{0};
        uint32_t tradableUnitsAdded{0};
        bool pvpLossIneligible{false};
    };

    // 与 go/shared/gameday 同一 UTC+8 05:00 换日契约；不读墙钟或本机时区，无法表示完整周期则失败。
    static bool ResolveDay(uint64_t nowMs, uint64_t& startMs, uint64_t& nextMs);
    // UTC+8 每周一 05:00（即 UTC 周日 21:00）切周契约；不读墙钟或本机时区。
    static bool ResolveWeek(uint64_t nowMs, uint64_t& startMs, uint64_t& nextMs);
    static bool IsEligibleBattle(const BattleSettlementData& settlement);
    // 返回本场景已知的所属账号阶段一累计可交易已用份数（>= 本角色 tradable_earned_total，封顶 60）。
    static uint32_t KnownAccountStage1TradableUsed(entt::entity player);
    // 纯候选准备，调用方必须处于收件箱已去重的应用路径，并将候选和全部结算效果同帧提交。
    // 不把 battle_id 当单调序号；永久去重由 PlayerInboxLedgerComp 的 battle 域水位承担。
    // accountStage1TradableUsed 为所属账号跨角色已知的阶段一可交易已用额（0 表示沿用单角色账本值）。
    static uint32_t PrepareBattleCredit(const ClientRequestLedgerComp& current,
        const BattleSettlementData& settlement, uint64_t nowMs, ClientRequestLedgerComp& candidate,
        BattleCreditDelta* deltaOut = nullptr, bool lightCardActive = false,
        uint32_t accountStage1TradableUsed = 0);

    static uint32_t Get(entt::entity player, uint64_t nowMs, GetEquipmentMaterialRewardResponse& response);
    static void Claim(entt::entity player, uint64_t nowMs,
        const ClaimEquipmentMaterialRewardRequest& request, ClaimEquipmentMaterialRewardResponse& response);

    // 账号级阶段一可交易配额异步同步生命周期钩子（scene EventLoop 线程调用，永不阻塞循环）。
    static void OnPlayerLoaded(entt::entity player, uint64_t nowMs);
    static void OnStage1TradableCredited(entt::entity player, uint64_t nowMs);
    static void OnPlayerDestroyed(Guid playerId);
    static void Tick(uint64_t nowMs);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
    using AccountQuotaSyncDone = std::function<void(bool callOk, const data_service::SyncAccountStage1TradableResponse* reply)>;
    using AccountQuotaSyncTransport = std::function<bool(const data_service::SyncAccountStage1TradableRequest& request, AccountQuotaSyncDone done)>;
    static void SetAccountQuotaSyncTransportForTest(AccountQuotaSyncTransport transport);
};
