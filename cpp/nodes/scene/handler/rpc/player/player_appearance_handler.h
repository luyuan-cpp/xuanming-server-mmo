#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_appearance.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneAppearanceClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetAppearanceInfo(entt::entity player,
        const ::GetAppearanceInfoRequest* request,
        ::GetAppearanceInfoResponse* response);
    static void SwitchAppearance(entt::entity player,
        const ::SwitchAppearanceRequest* request,
        ::SwitchAppearanceResponse* response);
    static void StartAppearanceTrial(entt::entity player,
        const ::StartAppearanceTrialRequest* request,
        ::StartAppearanceTrialResponse* response);
    static void CancelAppearanceTrial(entt::entity player,
        const ::CancelAppearanceTrialRequest* request,
        ::CancelAppearanceTrialResponse* response);
    static void NotifyActorAppearanceChanged(entt::entity player,
        const ::ActorAppearanceChangedS2C* request,
        ::Empty* response);
    static void NotifyAppearanceState(entt::entity player,
        const ::AppearanceStateNotify* request,
        ::Empty* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetAppearanceInfoResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetAppearanceInfo(player,
                static_cast<const ::GetAppearanceInfoRequest*>(request),
                static_cast<::GetAppearanceInfoResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SwitchAppearanceResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SwitchAppearance(player,
                static_cast<const ::SwitchAppearanceRequest*>(request),
                static_cast<::SwitchAppearanceResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::StartAppearanceTrialResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            StartAppearanceTrial(player,
                static_cast<const ::StartAppearanceTrialRequest*>(request),
                static_cast<::StartAppearanceTrialResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CancelAppearanceTrialResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CancelAppearanceTrial(player,
                static_cast<const ::CancelAppearanceTrialRequest*>(request),
                static_cast<::CancelAppearanceTrialResponse*>(response));
			}
            break;
        case 4:
			{
            NotifyActorAppearanceChanged(player,
                static_cast<const ::ActorAppearanceChangedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 5:
			{
            NotifyAppearanceState(player,
                static_cast<const ::AppearanceStateNotify*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        default:
            break;
        }
    }

};
