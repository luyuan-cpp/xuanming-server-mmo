#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_rewarded_ad.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneRewardedAdClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetRewardedAdInfo(entt::entity player,
        const ::GetRewardedAdInfoRequest* request,
        ::GetRewardedAdInfoResponse* response);
    static void BeginRewardedAdSession(entt::entity player,
        const ::BeginRewardedAdSessionRequest* request,
        ::BeginRewardedAdSessionResponse* response);
    static void ClaimRewardedAd(entt::entity player,
        const ::ClaimRewardedAdRequest* request,
        ::ClaimRewardedAdResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetRewardedAdInfoResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetRewardedAdInfo(player,
                static_cast<const ::GetRewardedAdInfoRequest*>(request),
                static_cast<::GetRewardedAdInfoResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BeginRewardedAdSessionResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BeginRewardedAdSession(player,
                static_cast<const ::BeginRewardedAdSessionRequest*>(request),
                static_cast<::BeginRewardedAdSessionResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimRewardedAdResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimRewardedAd(player,
                static_cast<const ::ClaimRewardedAdRequest*>(request),
                static_cast<::ClaimRewardedAdResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
