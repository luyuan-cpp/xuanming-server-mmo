#pragma once

#include <cstdint>
#include <functional>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

class BattleSettlementData;
class ClaimFestivalBeastRewardRequest;
class ClaimFestivalBeastRewardResponse;
class FestivalBeastBattleContext;
class FestivalBeastMaterialSummary;
class GetActivityListResponse;
class GetFestivalBeastDetailRequest;
class GetFestivalBeastDetailResponse;
class PlayerFestivalBeastComp;
class StartFestivalBeastChallengeRequest;
class StartFestivalBeastChallengeResponse;

namespace match {
class StartActivityBattleRequest;
class StartActivityBattleResponse;
}  // namespace match

// 瑞兽迎春限时活动场景服核心系统（docs/design/battle-festival-beast-implementation-contract.md）。
//
// 线程模型：全部入口在 scene 主 EventLoop 线程调用；异步 Match 回调仅按值捕获 player_id / request_id，
// 回调时重新查实体，绝不持有裸指针或跨异步缓存 entt::entity（AGENTS.md §11.7）。
class PlayerFestivalBeastSystem {
public:
    struct SettlementDelta {
        bool active{false};
        uint32_t activityInstanceId{0};
        std::string rewardIdentity;
        uint32_t winsBefore{0};
        uint32_t winsAfter{0};
        uint32_t requiredWins{0};
        bool newlyEligible{false};
        bool changed{false};
    };

    using PersistFn = bool (*)(entt::entity);
    using LaunchReplyFn = std::function<void(bool callOk, const match::StartActivityBattleResponse* reply)>;
    using LaunchSenderFn = std::function<bool(const match::StartActivityBattleRequest&, LaunchReplyFn)>;

    // 向活动大厅列表追加瑞兽迎春入口（含活动结束后的永久未领神兽资格提示）。
    static void AppendActivityCatalogEntry(entt::entity player, uint64_t nowMs, GetActivityListResponse& out);

    // 权威详情与跨设备恢复查询（在途开局、永久资格、领奖回执与共享材料账摘要）。
    static uint32_t GetDetail(entt::entity player, uint64_t nowMs,
                              const GetFestivalBeastDetailRequest& request,
                              GetFestivalBeastDetailResponse& response);

    // 发起挑战：校验开放窗口与存活出战宠、持久化在途开局并异步向 Match 提交权威准入。
    static uint32_t StartChallenge(entt::entity player, uint64_t nowMs,
                                   const StartFestivalBeastChallengeRequest& request,
                                   StartFestivalBeastChallengeResponse& response);

    // 领取永久资格神兽：同 UUID 重放原结果，异 UUID 并发收敛至同一 pet_id，号源失败不消费资格。
    static uint32_t ClaimReward(entt::entity player, uint64_t nowMs,
                                const ClaimFestivalBeastRewardRequest& request,
                                ClaimFestivalBeastRewardResponse& response);

    // PrepareBattle 建快照前的二次锁宠复核（拦截预检后换宠/收宠/死宠开局）。
    static uint32_t VerifyPrepareBattlePet(entt::entity player, const FestivalBeastBattleContext& ctx);

    // 备战取消或对局作废时清理匹配的在途开局状态，绝不改动既有胜场与永久资格。
    static void OnBattlePrepareCancelledOrVoid(entt::entity player, uint64_t battleId, uint64_t nowMs);

    // 战斗结算收件箱首次应用的纯候选制备：仅正常胜局加胜场（封顶 required_wins）并冻结永久资格。
    static uint32_t PrepareBattleCredit(const PlayerFestivalBeastComp& current,
                                        const BattleSettlementData& settlement,
                                        uint64_t nowMs,
                                        PlayerFestivalBeastComp& candidate,
                                        SettlementDelta& deltaOut);

    // 读取现有全游戏装备材料自选账摘要，供详情页与战果回显统一展示。
    static void BuildMaterialSummary(entt::entity player, uint64_t nowMs, FestivalBeastMaterialSummary& out);

    static void SetPersistForTest(PersistFn fn);
    static void SetLaunchSenderForTest(LaunchSenderFn fn);
};
