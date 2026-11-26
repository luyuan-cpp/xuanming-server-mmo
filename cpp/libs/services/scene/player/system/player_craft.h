#pragma once

#include <cstdint>
#include <string>
#include <entt/src/entt/entity/entity.hpp>

namespace muduo {
namespace net {
class EventLoop;
}
}

class CraftProxyStallInfo;
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
class ClaimScrollCollectionMilestoneRequest;
class ClaimScrollCollectionMilestoneResponse;
class BatchDismantleSkillBooksRequest;
class BatchDismantleSkillBooksResponse;
class PostSkillBookWantedOrderRequest;
class PostSkillBookWantedOrderResponse;
class CancelSkillBookWantedOrderRequest;
class CancelSkillBookWantedOrderResponse;
class ListSkillBookWantedOrdersRequest;
class ListSkillBookWantedOrdersResponse;
class FulfillSkillBookWantedOrderRequest;
class FulfillSkillBookWantedOrderResponse;
class AckCraftProxyDigestRequest;
class AckCraftProxyDigestResponse;
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
    static void ClaimScrollCollectionMilestone(entt::entity player,
                                               const ClaimScrollCollectionMilestoneRequest& request,
                                               ClaimScrollCollectionMilestoneResponse& response);
    static void BatchDismantleSkillBooks(entt::entity player, const BatchDismantleSkillBooksRequest& request,
                                         BatchDismantleSkillBooksResponse& response);
    static void PostWantedOrder(entt::entity player, const PostSkillBookWantedOrderRequest& request,
                                PostSkillBookWantedOrderResponse& response);
    static void CancelWantedOrder(entt::entity player, const CancelSkillBookWantedOrderRequest& request,
                                  CancelSkillBookWantedOrderResponse& response);
    static void ListWantedOrders(entt::entity player, const ListSkillBookWantedOrdersRequest& request,
                                 ListSkillBookWantedOrdersResponse& response);
    static void FulfillWantedOrder(entt::entity player, const FulfillSkillBookWantedOrderRequest& request,
                                   FulfillSkillBookWantedOrderResponse& response);
    static void AckProxyDigest(entt::entity player, const AckCraftProxyDigestRequest& request,
                               AckCraftProxyDigestResponse& response);

    // 跨节点/离线名匠代打共享摊位目录与口碑/热榜异步台账（控制集群 Redis Hash + 本地写穿/定时刷新缓存）。
    static std::string ProxyStallDirectoryKey(uint32_t zoneId);
    static std::string ProxyReputationLedgerKey(uint32_t zoneId);
    static void StartProxyStallDirectory(muduo::net::EventLoop* loop);
    static void StopProxyStallDirectory();
    static void SyncPlayerProxyStall(entt::entity player);
    static void RecordProxyOrderReputation(uint64_t masterPlayerId, uint32_t supremeDelta, uint32_t rareDelta,
                                           uint64_t netGoldEarned, const std::string& buyerName);
    static bool DrainPendingProxyReputation(entt::entity player);
    static bool ResolveSharedProxyStall(uint64_t buyerPlayerId, uint64_t masterPlayerId,
                                        bool requireForge, bool requireScroll,
                                        CraftProxyStallInfo& out);
    using PersistFn = bool (*)(entt::entity);
    static void SetPersistForTest(PersistFn fn);
    static void UpsertProxyStallForTest(const CraftProxyStallInfo& stall);
    static void RemoveProxyStallForTest(uint64_t masterPlayerId);
    static void ClearProxyStallDirectoryForTest();
};
