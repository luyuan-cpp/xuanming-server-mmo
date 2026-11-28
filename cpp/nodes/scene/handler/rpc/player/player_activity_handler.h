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
    static void GetTravelPassState(entt::entity player,
        const ::GetTravelPassStateRequest* request,
        ::GetTravelPassStateResponse* response);
    static void UnlockTravelPassPremium(entt::entity player,
        const ::UnlockTravelPassPremiumRequest* request,
        ::UnlockTravelPassPremiumResponse* response);
    static void ClaimTravelPassReward(entt::entity player,
        const ::ClaimTravelPassRewardRequest* request,
        ::ClaimTravelPassRewardResponse* response);
    static void EquipTravelPassCosmetic(entt::entity player,
        const ::EquipTravelPassCosmeticRequest* request,
        ::EquipTravelPassCosmeticResponse* response);
    static void SelectActiveTravelPass(entt::entity player,
        const ::SelectActiveTravelPassRequest* request,
        ::SelectActiveTravelPassResponse* response);

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
        case 5:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetTravelPassStateResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetTravelPassState(player,
                static_cast<const ::GetTravelPassStateRequest*>(request),
                static_cast<::GetTravelPassStateResponse*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::UnlockTravelPassPremiumResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            UnlockTravelPassPremium(player,
                static_cast<const ::UnlockTravelPassPremiumRequest*>(request),
                static_cast<::UnlockTravelPassPremiumResponse*>(response));
			}
            break;
        case 7:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimTravelPassRewardResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimTravelPassReward(player,
                static_cast<const ::ClaimTravelPassRewardRequest*>(request),
                static_cast<::ClaimTravelPassRewardResponse*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipTravelPassCosmeticResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipTravelPassCosmetic(player,
                static_cast<const ::EquipTravelPassCosmeticRequest*>(request),
                static_cast<::EquipTravelPassCosmeticResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SelectActiveTravelPassResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SelectActiveTravelPass(player,
                static_cast<const ::SelectActiveTravelPassRequest*>(request),
                static_cast<::SelectActiveTravelPassResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
