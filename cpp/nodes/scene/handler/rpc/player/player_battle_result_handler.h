#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_battle_result.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneBattleResultClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetRecentBattleResults(entt::entity player,
        const ::GetRecentBattleResultsRequest* request,
        ::GetRecentBattleResultsResponse* response);
    static void AckBattleResults(entt::entity player,
        const ::AckBattleResultsRequest* request,
        ::AckBattleResultsResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetRecentBattleResultsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetRecentBattleResults(player,
                static_cast<const ::GetRecentBattleResultsRequest*>(request),
                static_cast<::GetRecentBattleResultsResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AckBattleResultsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AckBattleResults(player,
                static_cast<const ::AckBattleResultsRequest*>(request),
                static_cast<::AckBattleResultsResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
