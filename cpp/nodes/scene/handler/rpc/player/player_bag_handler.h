#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_bag.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneBagClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetBag(entt::entity player,
        const ::GetBagRequest* request,
        ::GetBagResponse* response);
    static void SortBag(entt::entity player,
        const ::SortBagRequest* request,
        ::SortBagResponse* response);
    static void EquipItem(entt::entity player,
        const ::EquipItemRequest* request,
        ::EquipItemResponse* response);
    static void UnequipItem(entt::entity player,
        const ::UnequipItemRequest* request,
        ::UnequipItemResponse* response);
    static void GmGrantItem(entt::entity player,
        const ::GmGrantItemRequest* request,
        ::GmGrantItemResponse* response);
    static void EnhanceEquip(entt::entity player,
        const ::EnhanceEquipRequest* request,
        ::EnhanceEquipResponse* response);
    static void InlayGem(entt::entity player,
        const ::InlayGemRequest* request,
        ::InlayGemResponse* response);
    static void RemoveGem(entt::entity player,
        const ::RemoveGemRequest* request,
        ::RemoveGemResponse* response);
    static void RetrieveTemporaryItems(entt::entity player,
        const ::RetrieveTemporaryItemsRequest* request,
        ::RetrieveTemporaryItemsResponse* response);
    static void GetEquipmentCraftRules(entt::entity player,
        const ::GetEquipmentCraftRulesRequest* request,
        ::GetEquipmentCraftRulesResponse* response);
    static void PreviewEquipmentCraft(entt::entity player,
        const ::PreviewEquipmentCraftRequest* request,
        ::PreviewEquipmentCraftResponse* response);
    static void CommitEquipmentCraft(entt::entity player,
        const ::CommitEquipmentCraftRequest* request,
        ::CommitEquipmentCraftResponse* response);
    static void GetEquipmentMaterialReward(entt::entity player,
        const ::GetEquipmentMaterialRewardRequest* request,
        ::GetEquipmentMaterialRewardResponse* response);
    static void ClaimEquipmentMaterialReward(entt::entity player,
        const ::ClaimEquipmentMaterialRewardRequest* request,
        ::ClaimEquipmentMaterialRewardResponse* response);
    static void GetEquipLoadouts(entt::entity player,
        const ::GetEquipLoadoutsRequest* request,
        ::GetEquipLoadoutsResponse* response);
    static void SaveEquipLoadout(entt::entity player,
        const ::SaveEquipLoadoutRequest* request,
        ::SaveEquipLoadoutResponse* response);
    static void SwitchEquipLoadout(entt::entity player,
        const ::SwitchEquipLoadoutRequest* request,
        ::SwitchEquipLoadoutResponse* response);
    static void UnlockEquipLoadout(entt::entity player,
        const ::UnlockEquipLoadoutRequest* request,
        ::UnlockEquipLoadoutResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetBagResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetBag(player,
                static_cast<const ::GetBagRequest*>(request),
                static_cast<::GetBagResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SortBagResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SortBag(player,
                static_cast<const ::SortBagRequest*>(request),
                static_cast<::SortBagResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipItemResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipItem(player,
                static_cast<const ::EquipItemRequest*>(request),
                static_cast<::EquipItemResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::UnequipItemResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            UnequipItem(player,
                static_cast<const ::UnequipItemRequest*>(request),
                static_cast<::UnequipItemResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GmGrantItemResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GmGrantItem(player,
                static_cast<const ::GmGrantItemRequest*>(request),
                static_cast<::GmGrantItemResponse*>(response));
			}
            break;
        case 5:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EnhanceEquipResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EnhanceEquip(player,
                static_cast<const ::EnhanceEquipRequest*>(request),
                static_cast<::EnhanceEquipResponse*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::InlayGemResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            InlayGem(player,
                static_cast<const ::InlayGemRequest*>(request),
                static_cast<::InlayGemResponse*>(response));
			}
            break;
        case 7:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RemoveGemResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RemoveGem(player,
                static_cast<const ::RemoveGemRequest*>(request),
                static_cast<::RemoveGemResponse*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RetrieveTemporaryItemsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RetrieveTemporaryItems(player,
                static_cast<const ::RetrieveTemporaryItemsRequest*>(request),
                static_cast<::RetrieveTemporaryItemsResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetEquipmentCraftRulesResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetEquipmentCraftRules(player,
                static_cast<const ::GetEquipmentCraftRulesRequest*>(request),
                static_cast<::GetEquipmentCraftRulesResponse*>(response));
			}
            break;
        case 10:
			{
            static_assert(std::is_same_v<decltype(std::declval<::PreviewEquipmentCraftResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            PreviewEquipmentCraft(player,
                static_cast<const ::PreviewEquipmentCraftRequest*>(request),
                static_cast<::PreviewEquipmentCraftResponse*>(response));
			}
            break;
        case 11:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CommitEquipmentCraftResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CommitEquipmentCraft(player,
                static_cast<const ::CommitEquipmentCraftRequest*>(request),
                static_cast<::CommitEquipmentCraftResponse*>(response));
			}
            break;
        case 12:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetEquipmentMaterialRewardResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetEquipmentMaterialReward(player,
                static_cast<const ::GetEquipmentMaterialRewardRequest*>(request),
                static_cast<::GetEquipmentMaterialRewardResponse*>(response));
			}
            break;
        case 13:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimEquipmentMaterialRewardResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimEquipmentMaterialReward(player,
                static_cast<const ::ClaimEquipmentMaterialRewardRequest*>(request),
                static_cast<::ClaimEquipmentMaterialRewardResponse*>(response));
			}
            break;
        case 14:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetEquipLoadoutsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetEquipLoadouts(player,
                static_cast<const ::GetEquipLoadoutsRequest*>(request),
                static_cast<::GetEquipLoadoutsResponse*>(response));
			}
            break;
        case 15:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SaveEquipLoadoutResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SaveEquipLoadout(player,
                static_cast<const ::SaveEquipLoadoutRequest*>(request),
                static_cast<::SaveEquipLoadoutResponse*>(response));
			}
            break;
        case 16:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SwitchEquipLoadoutResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SwitchEquipLoadout(player,
                static_cast<const ::SwitchEquipLoadoutRequest*>(request),
                static_cast<::SwitchEquipLoadoutResponse*>(response));
			}
            break;
        case 17:
			{
            static_assert(std::is_same_v<decltype(std::declval<::UnlockEquipLoadoutResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            UnlockEquipLoadout(player,
                static_cast<const ::UnlockEquipLoadoutRequest*>(request),
                static_cast<::UnlockEquipLoadoutResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
