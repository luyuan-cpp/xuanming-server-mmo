#pragma once

#include <cstdint>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"

class BattleSettlementData;
class ClaimTrialJournalMilestoneRequest;
class ClaimTrialJournalMilestoneResponse;
class GetTrialJournalResponse;
class PlayerTrialJournalComp;
class RedeemBoundArtifactVoucherRequest;
class RedeemBoundArtifactVoucherResponse;

// 五行试炼志 V2 场景权威入口（设计见 docs/design/long-term-progression-v2.md）。
// 只在 scene EventLoop 线程调用：
//   - 读接口绝不创建持久组件，也不改档；
//   - 战斗结算通过 PrepareBattleCredit 在收件箱永久水位路径同帧点亮战法印与刷新最佳回合/复盘快照；
//   - 永久战法印与永久里程碑均走导表器 cfg_bit_index 紧凑 bytes 位图，天然透传未知高位 bit，统计严格遍历有效目录；
//   - 卷三（赛季轮换）按 season_id 隔离存放，不污染永久位图；
//   - 里程碑领取与自用法宝拓印兑换强制非 0 client_request_id 幂等与严格背包预检，且永不产出核心 660 装备材料。
class PlayerTrialJournalSystem {
public:
    struct SettlementDelta {
        uint32_t entryId{0};
        std::vector<uint32_t> newlyLitSealIds;
        bool newSoloBest{false};
        bool newCoopBest{false};
        uint32_t vowMask{0};
        bool newVowBest{false};
        uint64_t battleId{0};
        bool changed{false};
    };

    static bool ValidateTables();

    static bool HasJournalUpdate(const BattleSettlementData& settlement);
    // 普通难度任意首领试炼通关一次即永久解锁全部困难首领试炼（兼容既有试炼志普通通关战法印）。
    static bool HasClearedAnyNormalBossTrial(entt::entity player);

    // 纯候选准备：在 ApplySettlementToEntity 事务预检段调用，失败零副作用；
    // 成功后由调用方在 credit() 同步提交段与收件箱水位、近期战果同帧 Swap 落盘。
    static uint32_t PrepareBattleCredit(const PlayerTrialJournalComp& current,
                                        const BattleSettlementData& settlement,
                                        uint64_t nowMs,
                                        PlayerTrialJournalComp& candidate,
                                        SettlementDelta& delta);

    static uint32_t GetJournal(entt::entity player, uint64_t nowMs, GetTrialJournalResponse& response);

    static void ClaimMilestone(entt::entity player,
                               uint64_t nowMs,
                               const ClaimTrialJournalMilestoneRequest& request,
                               ClaimTrialJournalMilestoneResponse& response);

    static void RedeemArtifactVoucher(entt::entity player,
                                      uint64_t nowMs,
                                      const RedeemBoundArtifactVoucherRequest& request,
                                      RedeemBoundArtifactVoucherResponse& response);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
};
