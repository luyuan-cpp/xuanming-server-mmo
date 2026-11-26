#pragma once

#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class GetSkillCraftingStateResponse;
class CraftSkillBookRequest;
class CraftSkillBookResponse;
class LearnSkillBookRequest;
class LearnSkillBookResponse;
class DismantleSkillBookRequest;
class DismantleSkillBookResponse;
class SealSkillBookRequest;
class SealSkillBookResponse;
class UnsealSkillBookRequest;
class UnsealSkillBookResponse;
class SetCraftProxyStallRequest;
class SetCraftProxyStallResponse;
class ListCraftProxyStallsResponse;
class SkillCraftMasterySnapshot;

// 率土式分卷宗永久技能制书、轻量化手艺熟练度与主城原子代打摊位系统。
// 只在 scene 逻辑线程调用；读路径不懒建 PlayerCraftComp；写路径接入 ClientRequestLedgerComp 幂等防重。
class PlayerCraftSystem {
public:
    // 启动/热更校验 EquipAffixQuality 与 SkillScrollCraft 表，并硬锁永久技能稀世占比 <= 10%。
    static bool ValidateTables();

    // 首测战斗验收期默认关闭“须制书解锁”门槛，验收完毕后按配置开启；单测可通过接缝切换。
    static bool IsCraftUnlockGateEnabled();
    static void SetCraftUnlockGateForTest(bool enabled);

    using CraftRollFn = uint32_t (*)(uint32_t bound);
    static void SetCraftRollForTest(CraftRollFn fn);

    static bool IsSkillBookUnlocked(entt::entity player, uint32_t skillTableId);
    static void FillMasterySnapshots(entt::entity player, SkillCraftMasterySnapshot& forgeOut,
                                     SkillCraftMasterySnapshot& scrollOut);

    static uint32_t GetState(entt::entity player, GetSkillCraftingStateResponse& response);
    static void CraftSkillBook(entt::entity player, const CraftSkillBookRequest& request,
                               CraftSkillBookResponse& response);
    static void LearnSkillBook(entt::entity player, const LearnSkillBookRequest& request,
                               LearnSkillBookResponse& response);
    static void DismantleSkillBook(entt::entity player, const DismantleSkillBookRequest& request,
                                   DismantleSkillBookResponse& response);
    static void SealSkillBook(entt::entity player, const SealSkillBookRequest& request,
                              SealSkillBookResponse& response);
    static void UnsealSkillBook(entt::entity player, const UnsealSkillBookRequest& request,
                                UnsealSkillBookResponse& response);
    static void SetProxyStall(entt::entity player, const SetCraftProxyStallRequest& request,
                              SetCraftProxyStallResponse& response);
    static void ListProxyStalls(entt::entity player, ListCraftProxyStallsResponse& response);
};
