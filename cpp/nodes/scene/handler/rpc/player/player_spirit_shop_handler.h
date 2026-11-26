#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_spirit_shop.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneSpiritShopClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetSpiritShopInfo(entt::entity player,
        const ::GetSpiritShopInfoRequest* request,
        ::GetSpiritShopInfoResponse* response);
    static void BuySpiritShopGoods(entt::entity player,
        const ::BuySpiritShopGoodsRequest* request,
        ::BuySpiritShopGoodsResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetSpiritShopInfoResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetSpiritShopInfo(player,
                static_cast<const ::GetSpiritShopInfoRequest*>(request),
                static_cast<::GetSpiritShopInfoResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BuySpiritShopGoodsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BuySpiritShopGoods(player,
                static_cast<const ::BuySpiritShopGoodsRequest*>(request),
                static_cast<::BuySpiritShopGoodsResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
