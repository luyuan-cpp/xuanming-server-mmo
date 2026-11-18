#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_pet.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class ScenePetClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetPetList(entt::entity player,
        const ::GetPetListRequest* request,
        ::GetPetListResponse* response);
    static void SummonPet(entt::entity player,
        const ::SummonPetRequest* request,
        ::SummonPetResponse* response);
    static void RecallPet(entt::entity player,
        const ::RecallPetRequest* request,
        ::RecallPetResponse* response);
    static void AllocatePetPoints(entt::entity player,
        const ::AllocatePetPointsRequest* request,
        ::AllocatePetPointsResponse* response);
    static void ResetPetPoints(entt::entity player,
        const ::ResetPetPointsRequest* request,
        ::ResetPetPointsResponse* response);
    static void AutoAllocatePetPoints(entt::entity player,
        const ::AutoAllocatePetPointsRequest* request,
        ::AutoAllocatePetPointsResponse* response);
    static void RenamePet(entt::entity player,
        const ::RenamePetRequest* request,
        ::RenamePetResponse* response);
    static void NotifyPetListChanged(entt::entity player,
        const ::PetListChangedS2C* request,
        ::Empty* response);
    static void GmGrantPet(entt::entity player,
        const ::GmGrantPetRequest* request,
        ::GmGrantPetResponse* response);
    static void GetPetShop(entt::entity player,
        const ::GetPetShopRequest* request,
        ::GetPetShopResponse* response);
    static void BuyPet(entt::entity player,
        const ::BuyPetRequest* request,
        ::BuyPetResponse* response);
    static void GetPetEntries(entt::entity player,
        const ::GetPetEntriesRequest* request,
        ::GetPetEntriesResponse* response);
    static void PreviewPetEntries(entt::entity player,
        const ::PreviewPetEntriesRequest* request,
        ::PreviewPetEntriesResponse* response);
    static void SavePetEntries(entt::entity player,
        const ::SavePetEntriesRequest* request,
        ::SavePetEntriesResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetPetListResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetPetList(player,
                static_cast<const ::GetPetListRequest*>(request),
                static_cast<::GetPetListResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SummonPetResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SummonPet(player,
                static_cast<const ::SummonPetRequest*>(request),
                static_cast<::SummonPetResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RecallPetResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RecallPet(player,
                static_cast<const ::RecallPetRequest*>(request),
                static_cast<::RecallPetResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AllocatePetPointsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AllocatePetPoints(player,
                static_cast<const ::AllocatePetPointsRequest*>(request),
                static_cast<::AllocatePetPointsResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ResetPetPointsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ResetPetPoints(player,
                static_cast<const ::ResetPetPointsRequest*>(request),
                static_cast<::ResetPetPointsResponse*>(response));
			}
            break;
        case 5:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AutoAllocatePetPointsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AutoAllocatePetPoints(player,
                static_cast<const ::AutoAllocatePetPointsRequest*>(request),
                static_cast<::AutoAllocatePetPointsResponse*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RenamePetResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RenamePet(player,
                static_cast<const ::RenamePetRequest*>(request),
                static_cast<::RenamePetResponse*>(response));
			}
            break;
        case 7:
			{
            NotifyPetListChanged(player,
                static_cast<const ::PetListChangedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GmGrantPetResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GmGrantPet(player,
                static_cast<const ::GmGrantPetRequest*>(request),
                static_cast<::GmGrantPetResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetPetShopResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetPetShop(player,
                static_cast<const ::GetPetShopRequest*>(request),
                static_cast<::GetPetShopResponse*>(response));
			}
            break;
        case 10:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BuyPetResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BuyPet(player,
                static_cast<const ::BuyPetRequest*>(request),
                static_cast<::BuyPetResponse*>(response));
			}
            break;
        case 11:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetPetEntriesResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetPetEntries(player,
                static_cast<const ::GetPetEntriesRequest*>(request),
                static_cast<::GetPetEntriesResponse*>(response));
			}
            break;
        case 12:
			{
            static_assert(std::is_same_v<decltype(std::declval<::PreviewPetEntriesResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            PreviewPetEntries(player,
                static_cast<const ::PreviewPetEntriesRequest*>(request),
                static_cast<::PreviewPetEntriesResponse*>(response));
			}
            break;
        case 13:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SavePetEntriesResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SavePetEntries(player,
                static_cast<const ::SavePetEntriesRequest*>(request),
                static_cast<::SavePetEntriesResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
