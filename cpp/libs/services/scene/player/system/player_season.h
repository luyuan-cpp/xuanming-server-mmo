#pragma once

#include <cstdint>
#include <string>
#include <vector>
#include <entt/src/entt/entity/entity.hpp>
#include "services/battle/system/season_rules.h"

class AbandonWuxingTowerRunRequest;
class AbandonWuxingTowerRunResponse;
class BattleSettlementData;
class CatchupSeasonSealRequest;
class CatchupSeasonSealResponse;
class ChooseWuxingRealmBranchRequest;
class ChooseWuxingRealmBranchResponse;
class DeleteBattleBuildBookmarkRequest;
class DeleteBattleBuildBookmarkResponse;
class EquipSeasonSealRequest;
class EquipSeasonSealResponse;
class EquipWuxingFormationRequest;
class EquipWuxingFormationResponse;
class EquipmentEffects;
class ExpandSavedBuildSlotsRequest;
class ExpandSavedBuildSlotsResponse;
class GetSeasonStateResponse;
class GetWuxingTowerStateResponse;
class PickWuxingTowerSigilRequest;
class PickWuxingTowerSigilResponse;
class PlayerSeasonComp;
class ResolveWuxingRealmEventRequest;
class ResolveWuxingRealmEventResponse;
class SaveBattleBuildBookmarkRequest;
class SaveBattleBuildBookmarkResponse;
class SeasonSealEffect;
class SetRetroHonorTargetRequest;
class SetRetroHonorTargetResponse;
class StartWuxingTowerRunRequest;
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
                                              WuxingTowerBattleContext& outContext,
                                              uint32_t* outCharPresetId = nullptr,
                                              uint32_t* outPetPresetId = nullptr);

    // 水平扩展周榜 Redis Cluster 键规范（同一 {weekKey} hash-tag 确保 ZSET 与元信息 HASH 同 slot）。
    static std::string RealmLeaderboardZsetKey(uint32_t weekKey, bool useFreePreset);
    static std::string RealmLeaderboardMetaKey(uint32_t weekKey, bool useFreePreset);

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
                                    const StartWuxingTowerRunRequest& request,
                                    StartWuxingTowerRunResponse& response);
    static void StartWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                    StartWuxingTowerRunResponse& response);
    static void PickWuxingTowerSigil(entt::entity player, uint64_t nowMs,
                                     const PickWuxingTowerSigilRequest& request,
                                     PickWuxingTowerSigilResponse& response);
    static void AbandonWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                      const AbandonWuxingTowerRunRequest& request,
                                      AbandonWuxingTowerRunResponse& response);
    static void ChooseWuxingRealmBranch(entt::entity player, uint64_t nowMs,
                                        const ChooseWuxingRealmBranchRequest& request,
                                        ChooseWuxingRealmBranchResponse& response);
    static void ResolveWuxingRealmEvent(entt::entity player, uint64_t nowMs,
                                        const ResolveWuxingRealmEventRequest& request,
                                        ResolveWuxingRealmEventResponse& response);
    static void SaveBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                        const SaveBattleBuildBookmarkRequest& request,
                                        SaveBattleBuildBookmarkResponse& response);
    static void DeleteBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                          const DeleteBattleBuildBookmarkRequest& request,
                                          DeleteBattleBuildBookmarkResponse& response);
    static constexpr uint32_t kBaseSavedBuildCapacity = 12u;
    static constexpr uint32_t kSavedBuildExpandStep = 12u;
    static constexpr uint32_t kMaxSavedBuildCapacity = 48u;
    static constexpr uint64_t kSavedBuildExpandPriceDiamond = 60ULL;
    static void ExpandSavedBuildSlots(entt::entity player, uint64_t nowMs,
                                      const ExpandSavedBuildSlotsRequest& request,
                                      ExpandSavedBuildSlotsResponse& response);
    // B4 激励视频槽位 2：五行幻境 V2 待选奇策重随（M-7 硬锁每局 run_id 最多 1 次，0 资产产出）。
    static uint32_t RerollWuxingTowerOffersByAd(entt::entity player, uint32_t expectedRunIdLow32,
                                                uint64_t nowMs, std::vector<uint32_t>& outOffers,
                                                bool dryRun = false);

    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
    static void ResetTowerLeaderboardForTest();
};
