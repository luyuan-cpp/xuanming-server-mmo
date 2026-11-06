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
        default:
            break;
        }
    }

};
