#pragma once

#include "proto/scene/player_bag.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneBagClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetBag(entt::entity player,
        const ::GetBagRequest* request,
        ::GetBagResponse* response);
    static void SortBag(entt::entity player,
        const ::SortBagRequest* request,
        ::SortBagResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            GetBag(player,
                static_cast<const ::GetBagRequest*>(request),
                static_cast<::GetBagResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GetBagResponse*>(response));
			}
            break;
        case 1:
			{
            SortBag(player,
                static_cast<const ::SortBagRequest*>(request),
                static_cast<::SortBagResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::SortBagResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
