#pragma once

#include "proto/scene/player_mission.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneMissionClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetMissionList(entt::entity player,
        const ::GetMissionListRequest* request,
        ::GetMissionListResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            GetMissionList(player,
                static_cast<const ::GetMissionListRequest*>(request),
                static_cast<::GetMissionListResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GetMissionListResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
