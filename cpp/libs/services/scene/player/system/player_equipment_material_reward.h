#pragma once

#include <cstdint>
#include "entt/src/entt/entity/entity.hpp"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/scene/player_bag.pb.h"

// scene EventLoop 单写。额度只来自收件箱中首次应用的正式战斗结算，与其永久水位同快照落盘。
// 领取通过严格背包事务同时扣额度、记回执；revision 只随成功领取增加，入账不改变领取水位。
// APPLIED 仍须 durable 才能向玩家展示成功。
class PlayerEquipmentMaterialRewardSystem {
public:
    static constexpr uint32_t kDailyLimit = 20;
    static constexpr uint32_t kBattleUnits = 10;
    static constexpr uint32_t kCraftingMaterial = 3001;
    static constexpr uint32_t kEnhancementStone = 3002;

    // 与 go/shared/gameday 同一 UTC+8 05:00 换日契约；不读墙钟或本机时区，无法表示完整周期则失败。
    static bool ResolveDay(uint64_t nowMs, uint64_t& startMs, uint64_t& nextMs);
    static bool IsEligibleBattle(const BattleSettlementData& settlement);
    // 纯候选准备，调用方必须处于收件箱已去重的应用路径，并将候选和全部结算效果同帧提交。
    // 不把 battle_id 当单调序号；永久去重由 PlayerInboxLedgerComp 的 battle 域水位承担。
    static uint32_t PrepareBattleCredit(const ClientRequestLedgerComp& current,
        const BattleSettlementData& settlement, uint64_t nowMs, ClientRequestLedgerComp& candidate);

    static uint32_t Get(entt::entity player, uint64_t nowMs, GetEquipmentMaterialRewardResponse& response);
    static void Claim(entt::entity player, uint64_t nowMs,
        const ClaimEquipmentMaterialRewardRequest& request, ClaimEquipmentMaterialRewardResponse& response);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
};
