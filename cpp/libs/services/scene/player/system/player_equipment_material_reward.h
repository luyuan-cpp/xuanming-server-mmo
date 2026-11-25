#pragma once

#include <cstdint>
#include "entt/src/entt/entity/entity.hpp"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/scene/player_bag.pb.h"

// scene EventLoop 单写。额度只来自收件箱中首次应用的正式战斗结算，与其永久水位同快照落盘。
// 两阶段总账（合入 A）：
//   - 第一阶段（lifetime_earned < 660）：无每日/每周刷新，永久 660 份总池（首套 600 + 60 容错）；
//     其中胜局每满 10 份基础额包含至多 1 份可交易，第一阶段终身可交易上限 60 份，余量全绑定。
//   - 第二阶段（lifetime_earned >= 660）：自动切换为每周平滑 100 份全绑定池（UTC+8 周一 05:00 重置，防回拨），
//     供 +11 满配突破与二套平缓推进，0 免费可交易产出。
// 领取通过严格背包事务同时扣额度、记回执，可交易与绑定分堆入包；revision 只随成功领取增加，入账不改变领取水位。
// APPLIED 仍须 durable 才能向玩家展示成功。
class PlayerEquipmentMaterialRewardSystem {
public:
    static constexpr uint32_t kDailyLimit = 20;            // 兼容字段与单批领取上限
    static constexpr uint32_t kMaxClaimBatchUnits = 20;
    static constexpr uint32_t kMinClaimStepUnits = 5;
    static constexpr uint32_t kBattleUnits = 10;           // 单局基础份数
    static constexpr uint32_t kBonusUnits = 5;             // 困难 PVE / PVP 胜局额外绑定份数
    static constexpr uint32_t kLifetimeLimit = 660;        // 第一阶段首套永久总池
    static constexpr uint32_t kTradableLifetimeLimit = 60; // 第一阶段可交易终身总上限
    static constexpr uint32_t kWeeklyLimit = 100;          // 第二阶段每周全绑定平滑总池
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
    // 纯候选准备，调用方必须处于收件箱已去重的应用路径，并将候选和全部结算效果同帧提交。
    // 不把 battle_id 当单调序号；永久去重由 PlayerInboxLedgerComp 的 battle 域水位承担。
    static uint32_t PrepareBattleCredit(const ClientRequestLedgerComp& current,
        const BattleSettlementData& settlement, uint64_t nowMs, ClientRequestLedgerComp& candidate,
        BattleCreditDelta* deltaOut = nullptr);

    static uint32_t Get(entt::entity player, uint64_t nowMs, GetEquipmentMaterialRewardResponse& response);
    static void Claim(entt::entity player, uint64_t nowMs,
        const ClaimEquipmentMaterialRewardRequest& request, ClaimEquipmentMaterialRewardResponse& response);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
};
