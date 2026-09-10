#pragma once

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

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            GetActivityList(player,
                static_cast<const ::GetActivityListRequest*>(request),
                static_cast<::GetActivityListResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GetActivityListResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
