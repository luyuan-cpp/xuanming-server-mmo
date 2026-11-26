#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_afk_pass.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneAfkPassClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetAfkPass(entt::entity player,
        const ::GetAfkPassRequest* request,
        ::GetAfkPassResponse* response);
    static void BuyAfkPass(entt::entity player,
        const ::BuyAfkPassRequest* request,
        ::BuyAfkPassResponse* response);
    static void GmSetAfkPass(entt::entity player,
        const ::GmSetAfkPassRequest* request,
        ::GmSetAfkPassResponse* response);
    static void SetAfkPassSettings(entt::entity player,
        const ::SetAfkPassSettingsRequest* request,
        ::SetAfkPassSettingsResponse* response);
    static void AckHostedRecap(entt::entity player,
        const ::AckHostedRecapRequest* request,
        ::AckHostedRecapResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetAfkPassResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetAfkPass(player,
                static_cast<const ::GetAfkPassRequest*>(request),
                static_cast<::GetAfkPassResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BuyAfkPassResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BuyAfkPass(player,
                static_cast<const ::BuyAfkPassRequest*>(request),
                static_cast<::BuyAfkPassResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GmSetAfkPassResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GmSetAfkPass(player,
                static_cast<const ::GmSetAfkPassRequest*>(request),
                static_cast<::GmSetAfkPassResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SetAfkPassSettingsResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SetAfkPassSettings(player,
                static_cast<const ::SetAfkPassSettingsRequest*>(request),
                static_cast<::SetAfkPassSettingsResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AckHostedRecapResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AckHostedRecap(player,
                static_cast<const ::AckHostedRecapRequest*>(request),
                static_cast<::AckHostedRecapResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
