#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_activity.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneActivityClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetActivityList(entt::entity player,
        const ::GetActivityListRequest* request,
        ::GetActivityListResponse* response);
    static void ClaimActivityVitalityReward(entt::entity player,
        const ::ClaimActivityVitalityRewardRequest* request,
        ::ClaimActivityVitalityRewardResponse* response);
    static void GetFestivalBeastDetail(entt::entity player,
        const ::GetFestivalBeastDetailRequest* request,
        ::GetFestivalBeastDetailResponse* response);
    static void StartFestivalBeastChallenge(entt::entity player,
        const ::StartFestivalBeastChallengeRequest* request,
        ::StartFestivalBeastChallengeResponse* response);
    static void ClaimFestivalBeastReward(entt::entity player,
        const ::ClaimFestivalBeastRewardRequest* request,
        ::ClaimFestivalBeastRewardResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetActivityListResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetActivityList(player,
                static_cast<const ::GetActivityListRequest*>(request),
                static_cast<::GetActivityListResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimActivityVitalityRewardResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimActivityVitalityReward(player,
                static_cast<const ::ClaimActivityVitalityRewardRequest*>(request),
                static_cast<::ClaimActivityVitalityRewardResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetFestivalBeastDetailResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetFestivalBeastDetail(player,
                static_cast<const ::GetFestivalBeastDetailRequest*>(request),
                static_cast<::GetFestivalBeastDetailResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::StartFestivalBeastChallengeResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            StartFestivalBeastChallenge(player,
                static_cast<const ::StartFestivalBeastChallengeRequest*>(request),
                static_cast<::StartFestivalBeastChallengeResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimFestivalBeastRewardResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimFestivalBeastReward(player,
                static_cast<const ::ClaimFestivalBeastRewardRequest*>(request),
                static_cast<::ClaimFestivalBeastRewardResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
