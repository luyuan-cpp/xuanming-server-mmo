#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_season.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneSeasonClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetSeasonState(entt::entity player,
        const ::GetSeasonStateRequest* request,
        ::GetSeasonStateResponse* response);
    static void EquipSeasonSeal(entt::entity player,
        const ::EquipSeasonSealRequest* request,
        ::EquipSeasonSealResponse* response);
    static void CatchupSeasonSeal(entt::entity player,
        const ::CatchupSeasonSealRequest* request,
        ::CatchupSeasonSealResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetSeasonStateResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetSeasonState(player,
                static_cast<const ::GetSeasonStateRequest*>(request),
                static_cast<::GetSeasonStateResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipSeasonSealResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipSeasonSeal(player,
                static_cast<const ::EquipSeasonSealRequest*>(request),
                static_cast<::EquipSeasonSealResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CatchupSeasonSealResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CatchupSeasonSeal(player,
                static_cast<const ::CatchupSeasonSealRequest*>(request),
                static_cast<::CatchupSeasonSealResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
