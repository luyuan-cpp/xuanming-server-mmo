#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_movement.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneMovementClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void MoveStart(entt::entity player,
        const ::MoveStartC2S* request,
        ::Empty* response);
    static void MoveStop(entt::entity player,
        const ::MoveStopC2S* request,
        ::Empty* response);
    static void MoveSync(entt::entity player,
        const ::MoveSyncC2S* request,
        ::Empty* response);
    static void TeleportRequest(entt::entity player,
        const ::TeleportRequestC2S* request,
        ::TeleportRequestC2SResponse* response);
    static void NotifyMoveAck(entt::entity player,
        const ::MoveAckS2C* request,
        ::Empty* response);
    static void NotifyActorMove(entt::entity player,
        const ::ActorMoveS2C* request,
        ::Empty* response);
    static void NotifyActorMoveList(entt::entity player,
        const ::ActorMoveListS2C* request,
        ::Empty* response);
    static void NotifyTeleport(entt::entity player,
        const ::TeleportS2C* request,
        ::Empty* response);
    static void TeamFollow(entt::entity player,
        const ::TeamFollowC2S* request,
        ::Empty* response);
    static void NotifyTeamFollowState(entt::entity player,
        const ::TeamFollowStateS2C* request,
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
            MoveStart(player,
                static_cast<const ::MoveStartC2S*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 1:
			{
            MoveStop(player,
                static_cast<const ::MoveStopC2S*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 2:
			{
            MoveSync(player,
                static_cast<const ::MoveSyncC2S*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::TeleportRequestC2SResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            TeleportRequest(player,
                static_cast<const ::TeleportRequestC2S*>(request),
                static_cast<::TeleportRequestC2SResponse*>(response));
			}
            break;
        case 4:
			{
            NotifyMoveAck(player,
                static_cast<const ::MoveAckS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 5:
			{
            NotifyActorMove(player,
                static_cast<const ::ActorMoveS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 6:
			{
            NotifyActorMoveList(player,
                static_cast<const ::ActorMoveListS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 7:
			{
            NotifyTeleport(player,
                static_cast<const ::TeleportS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 8:
			{
            TeamFollow(player,
                static_cast<const ::TeamFollowC2S*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 9:
			{
            NotifyTeamFollowState(player,
                static_cast<const ::TeamFollowStateS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        default:
            break;
        }
    }

};
