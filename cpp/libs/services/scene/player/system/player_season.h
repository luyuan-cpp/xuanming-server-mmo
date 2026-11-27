#pragma once

#include <cstdint>
#include <vector>
#include <entt/src/entt/entity/entity.hpp>
#include "services/battle/system/season_rules.h"

class AbandonWuxingTowerRunRequest;
class AbandonWuxingTowerRunResponse;
class BattleSettlementData;
class CatchupSeasonSealRequest;
class CatchupSeasonSealResponse;
class DeleteBattleBuildBookmarkRequest;
class DeleteBattleBuildBookmarkResponse;
class EquipSeasonSealRequest;
class EquipSeasonSealResponse;
class EquipWuxingFormationRequest;
class EquipWuxingFormationResponse;
class EquipmentEffects;
class GetSeasonStateResponse;
class GetWuxingTowerStateResponse;
class PickWuxingTowerSigilRequest;
class PickWuxingTowerSigilResponse;
class PlayerSeasonComp;
class SaveBattleBuildBookmarkRequest;
class SaveBattleBuildBookmarkResponse;
class SeasonSealEffect;
class SetRetroHonorTargetRequest;
class SetRetroHonorTargetResponse;
class StartWuxingTowerRunResponse;
class WuxingTowerBattleContext;

// 赛季技能与单槽赛季印（方案 A）的场景权威入口（设计见 docs/design/season-skills-and-seal-design.md）。
// 同时承接五行阵法、五行幻境周常爬塔与战报配装收藏夹的场景权威读写（docs/design/wuxing-gameplay-expansions-v1.md）。
// 只在 scene EventLoop 线程调用：
//   - 读接口绝不创建持久组件；
//   - 赛季印轻属性（仅气血与防御，<= 5%）与机制通过 CollectEffects 进入面板重算和战斗快照；
//   - 正常排位 PVP 与正规 PVE 胜利在战斗结算帧通过 PrepareBattleCredit 原子累计进度点；
//   - 小额追赶强制非 0 client_request_id 幂等与报价防呆，与免费玩家严格同封顶上限。
class PlayerSeasonSystem {
public:
    static bool ValidateTables();
    static uint32_t ActiveSeasonId(uint64_t nowMs);
    static uint32_t ActiveWeeklyAffixId(uint64_t nowMs);
    static uint32_t EquippedFormationId(entt::entity player);

    static bool IsEligibleBattle(const BattleSettlementData& settlement);
    static uint32_t PrepareBattleCredit(const PlayerSeasonComp& current,
                                        const BattleSettlementData& settlement,
                                        uint64_t nowMs,
                                        PlayerSeasonComp& candidate,
                                        bool& pointsIncreased,
                                        bool* outTowerChanged = nullptr,
                                        seasonrules::BattleCreditDelta* outDelta = nullptr);
    static void OnTowerSettlementApplied(entt::entity player,
                                         const PlayerSeasonComp& current,
                                         uint64_t nowMs);
    static uint32_t PrepareWuxingTowerContext(entt::entity player,
                                              uint64_t nowMs,
                                              uint32_t battleConfigId,
                                              WuxingTowerBattleContext& outContext);

    // 汇总当前活跃赛季单槽赛季印的轻属性与机制到 EquipmentEffects（无活跃赛季或组件缺失则留空）。
    static void CollectEffects(entt::entity player, uint64_t nowMs, EquipmentEffects& effects);
    // 论剑 V3 战前 15 秒明牌变阵：收集当前唯一活跃赛季下全部合法赛季印快照（按 seal_id 升序）。
    static void CollectAvailableSeals(uint64_t nowMs, std::vector<SeasonSealEffect>& out);

    static uint32_t GetState(entt::entity player, uint64_t nowMs, GetSeasonStateResponse& response);
    static void EquipSeal(entt::entity player, uint64_t nowMs,
                          const EquipSeasonSealRequest& request,
                          EquipSeasonSealResponse& response);
    static void Catchup(entt::entity player, uint64_t nowMs,
                        const CatchupSeasonSealRequest& request,
                        CatchupSeasonSealResponse& response);
    static void SetRetroHonorTarget(entt::entity player, uint64_t nowMs,
                                    const SetRetroHonorTargetRequest& request,
                                    SetRetroHonorTargetResponse& response);

    static void EquipFormation(entt::entity player, uint64_t nowMs,
                               const EquipWuxingFormationRequest& request,
                               EquipWuxingFormationResponse& response);
    // 流派方案一键联动切阵接缝（只改已加载组件、不重复落盘，由调用方统一 RequestKeyProgressSave）。
    static uint32_t TryEquipLinkedFormation(entt::entity player, uint32_t formationId);
    static uint32_t GetWuxingTowerState(entt::entity player, uint64_t nowMs,
                                        GetWuxingTowerStateResponse& response);
    static void StartWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                    StartWuxingTowerRunResponse& response);
    static void PickWuxingTowerSigil(entt::entity player, uint64_t nowMs,
                                     const PickWuxingTowerSigilRequest& request,
                                     PickWuxingTowerSigilResponse& response);
    static void AbandonWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                      const AbandonWuxingTowerRunRequest& request,
                                      AbandonWuxingTowerRunResponse& response);
    static void SaveBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                        const SaveBattleBuildBookmarkRequest& request,
                                        SaveBattleBuildBookmarkResponse& response);
    static void DeleteBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                          const DeleteBattleBuildBookmarkRequest& request,
                                          DeleteBattleBuildBookmarkResponse& response);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
    static void ResetTowerLeaderboardForTest();
};
