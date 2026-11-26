#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_skill.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneSkillClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void ReleaseSkill(entt::entity player,
        const ::ReleaseSkillRequest* request,
        ::ReleaseSkillResponse* response);
    static void NotifySkillUsed(entt::entity player,
        const ::SkillUsedS2C* request,
        ::Empty* response);
    static void NotifySkillInterrupted(entt::entity player,
        const ::SkillInterruptedS2C* request,
        ::Empty* response);
    static void ListSkills(entt::entity player,
        const ::ListSkillsRequest* request,
        ::ListSkillsResponse* response);
    static void UpgradeSkill(entt::entity player,
        const ::UpgradeSkillRequest* request,
        ::UpgradeSkillResponse* response);
    static void GetSkillCraftingState(entt::entity player,
        const ::GetSkillCraftingStateRequest* request,
        ::GetSkillCraftingStateResponse* response);
    static void CraftSkillBook(entt::entity player,
        const ::CraftSkillBookRequest* request,
        ::CraftSkillBookResponse* response);
    static void LearnSkillBook(entt::entity player,
        const ::LearnSkillBookRequest* request,
        ::LearnSkillBookResponse* response);
    static void DismantleSkillBook(entt::entity player,
        const ::DismantleSkillBookRequest* request,
        ::DismantleSkillBookResponse* response);
    static void SetCraftProxyStall(entt::entity player,
        const ::SetCraftProxyStallRequest* request,
        ::SetCraftProxyStallResponse* response);
    static void ListCraftProxyStalls(entt::entity player,
        const ::ListCraftProxyStallsRequest* request,
        ::ListCraftProxyStallsResponse* response);
    static void SaveSkillLoadout(entt::entity player,
        const ::SaveSkillLoadoutRequest* request,
        ::SaveSkillLoadoutResponse* response);
    static void SwitchSkillLoadout(entt::entity player,
        const ::SwitchSkillLoadoutRequest* request,
        ::SwitchSkillLoadoutResponse* response);
    static void SealSkillBook(entt::entity player,
        const ::SealSkillBookRequest* request,
        ::SealSkillBookResponse* response);
    static void UnsealSkillBook(entt::entity player,
        const ::UnsealSkillBookRequest* request,
        ::UnsealSkillBookResponse* response);
    static void ClaimScrollCollectionMilestone(entt::entity player,
        const ::ClaimScrollCollectionMilestoneRequest* request,
        ::ClaimScrollCollectionMilestoneResponse* response);
    static void BatchDismantleSkillBooks(entt::entity player,
        const ::BatchDismantleSkillBooksRequest* request,
        ::BatchDismantleSkillBooksResponse* response);
    static void PostSkillBookWantedOrder(entt::entity player,
        const ::PostSkillBookWantedOrderRequest* request,
        ::PostSkillBookWantedOrderResponse* response);
    static void CancelSkillBookWantedOrder(entt::entity player,
        const ::CancelSkillBookWantedOrderRequest* request,
        ::CancelSkillBookWantedOrderResponse* response);
    static void ListSkillBookWantedOrders(entt::entity player,
        const ::ListSkillBookWantedOrdersRequest* request,
        ::ListSkillBookWantedOrdersResponse* response);
    static void FulfillSkillBookWantedOrder(entt::entity player,
        const ::FulfillSkillBookWantedOrderRequest* request,
        ::FulfillSkillBookWantedOrderResponse* response);
    static void RentSkillLoadoutSlots(entt::entity player,
        const ::RentSkillLoadoutSlotsRequest* request,
        ::RentSkillLoadoutSlotsResponse* response);
    static void AckCraftProxyDigest(entt::entity player,
        const ::AckCraftProxyDigestRequest* request,
        ::AckCraftProxyDigestResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ReleaseSkillResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ReleaseSkill(player,
                static_cast<const ::ReleaseSkillRequest*>(request),
                static_cast<::ReleaseSkillResponse*>(response));
			}
            break;
        case 1:
			{
            NotifySkillUsed(player,
                static_cast<const ::SkillUsedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 2:
			{
            NotifySkillInterrupted(player,
                static_cast<const ::SkillInterruptedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ListSkillsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ListSkills(player,
                static_cast<const ::ListSkillsRequest*>(request),
                static_cast<::ListSkillsResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::UpgradeSkillResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            UpgradeSkill(player,
                static_cast<const ::UpgradeSkillRequest*>(request),
                static_cast<::UpgradeSkillResponse*>(response));
			}
            break;
        case 5:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetSkillCraftingStateResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetSkillCraftingState(player,
                static_cast<const ::GetSkillCraftingStateRequest*>(request),
                static_cast<::GetSkillCraftingStateResponse*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CraftSkillBookResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CraftSkillBook(player,
                static_cast<const ::CraftSkillBookRequest*>(request),
                static_cast<::CraftSkillBookResponse*>(response));
			}
            break;
        case 7:
			{
            static_assert(std::is_same_v<decltype(std::declval<::LearnSkillBookResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            LearnSkillBook(player,
                static_cast<const ::LearnSkillBookRequest*>(request),
                static_cast<::LearnSkillBookResponse*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::DismantleSkillBookResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            DismantleSkillBook(player,
                static_cast<const ::DismantleSkillBookRequest*>(request),
                static_cast<::DismantleSkillBookResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SetCraftProxyStallResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SetCraftProxyStall(player,
                static_cast<const ::SetCraftProxyStallRequest*>(request),
                static_cast<::SetCraftProxyStallResponse*>(response));
			}
            break;
        case 10:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ListCraftProxyStallsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ListCraftProxyStalls(player,
                static_cast<const ::ListCraftProxyStallsRequest*>(request),
                static_cast<::ListCraftProxyStallsResponse*>(response));
			}
            break;
        case 11:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SaveSkillLoadoutResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SaveSkillLoadout(player,
                static_cast<const ::SaveSkillLoadoutRequest*>(request),
                static_cast<::SaveSkillLoadoutResponse*>(response));
			}
            break;
        case 12:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SwitchSkillLoadoutResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SwitchSkillLoadout(player,
                static_cast<const ::SwitchSkillLoadoutRequest*>(request),
                static_cast<::SwitchSkillLoadoutResponse*>(response));
			}
            break;
        case 13:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SealSkillBookResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SealSkillBook(player,
                static_cast<const ::SealSkillBookRequest*>(request),
                static_cast<::SealSkillBookResponse*>(response));
			}
            break;
        case 14:
			{
            static_assert(std::is_same_v<decltype(std::declval<::UnsealSkillBookResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            UnsealSkillBook(player,
                static_cast<const ::UnsealSkillBookRequest*>(request),
                static_cast<::UnsealSkillBookResponse*>(response));
			}
            break;
        case 15:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimScrollCollectionMilestoneResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimScrollCollectionMilestone(player,
                static_cast<const ::ClaimScrollCollectionMilestoneRequest*>(request),
                static_cast<::ClaimScrollCollectionMilestoneResponse*>(response));
			}
            break;
        case 16:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BatchDismantleSkillBooksResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BatchDismantleSkillBooks(player,
                static_cast<const ::BatchDismantleSkillBooksRequest*>(request),
                static_cast<::BatchDismantleSkillBooksResponse*>(response));
			}
            break;
        case 17:
			{
            static_assert(std::is_same_v<decltype(std::declval<::PostSkillBookWantedOrderResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            PostSkillBookWantedOrder(player,
                static_cast<const ::PostSkillBookWantedOrderRequest*>(request),
                static_cast<::PostSkillBookWantedOrderResponse*>(response));
			}
            break;
        case 18:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CancelSkillBookWantedOrderResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CancelSkillBookWantedOrder(player,
                static_cast<const ::CancelSkillBookWantedOrderRequest*>(request),
                static_cast<::CancelSkillBookWantedOrderResponse*>(response));
			}
            break;
        case 19:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ListSkillBookWantedOrdersResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ListSkillBookWantedOrders(player,
                static_cast<const ::ListSkillBookWantedOrdersRequest*>(request),
                static_cast<::ListSkillBookWantedOrdersResponse*>(response));
			}
            break;
        case 20:
			{
            static_assert(std::is_same_v<decltype(std::declval<::FulfillSkillBookWantedOrderResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            FulfillSkillBookWantedOrder(player,
                static_cast<const ::FulfillSkillBookWantedOrderRequest*>(request),
                static_cast<::FulfillSkillBookWantedOrderResponse*>(response));
			}
            break;
        case 21:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RentSkillLoadoutSlotsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RentSkillLoadoutSlots(player,
                static_cast<const ::RentSkillLoadoutSlotsRequest*>(request),
                static_cast<::RentSkillLoadoutSlotsResponse*>(response));
			}
            break;
        case 22:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AckCraftProxyDigestResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AckCraftProxyDigest(player,
                static_cast<const ::AckCraftProxyDigestRequest*>(request),
                static_cast<::AckCraftProxyDigestResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
