#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/client_player_common.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneClientPlayerCommonHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void SendTipToClient(entt::entity player,
        const ::TipInfoMessage* request,
        ::Empty* response);
    static void KickPlayer(entt::entity player,
        const ::GameKickPlayerRequest* request,
        ::Empty* response);
    static void RedirectToGate(entt::entity player,
        const ::RedirectToGateNotify* request,
        ::Empty* response);
    static void RenamePlayer(entt::entity player,
        const ::RenamePlayerRequest* request,
        ::RenamePlayerResponse* response);
    static void NotifyPlayerRenamed(entt::entity player,
        const ::PlayerRenamedNotify* request,
        ::Empty* response);
    static void NotifySelfActorStateFlags(entt::entity player,
        const ::SelfActorStateFlagsS2C* request,
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
            SendTipToClient(player,
                static_cast<const ::TipInfoMessage*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 1:
			{
            KickPlayer(player,
                static_cast<const ::GameKickPlayerRequest*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 2:
			{
            RedirectToGate(player,
                static_cast<const ::RedirectToGateNotify*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RenamePlayerResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RenamePlayer(player,
                static_cast<const ::RenamePlayerRequest*>(request),
                static_cast<::RenamePlayerResponse*>(response));
			}
            break;
        case 4:
			{
            NotifyPlayerRenamed(player,
                static_cast<const ::PlayerRenamedNotify*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 5:
			{
            NotifySelfActorStateFlags(player,
                static_cast<const ::SelfActorStateFlagsS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        default:
            break;
        }
    }

};
