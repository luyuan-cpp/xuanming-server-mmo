#pragma once

#include "proto/scene/player_scene.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneSceneClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void EnterScene(entt::entity player,
        const ::EnterSceneC2SRequest* request,
        ::EnterSceneC2SResponse* response);
    static void NotifyEnterScene(entt::entity player,
        const ::EnterSceneS2C* request,
        ::Empty* response);
    static void SceneInfoC2S(entt::entity player,
        const ::SceneInfoRequest* request,
        ::Empty* response);
    static void NotifySceneInfo(entt::entity player,
        const ::SceneInfoS2C* request,
        ::Empty* response);
    static void NotifyActorCreate(entt::entity player,
        const ::ActorCreateS2C* request,
        ::Empty* response);
    static void NotifyActorDestroy(entt::entity player,
        const ::ActorDestroyS2C* request,
        ::Empty* response);
    static void NotifyActorListCreate(entt::entity player,
        const ::ActorListCreateS2C* request,
        ::Empty* response);
    static void NotifyActorListDestroy(entt::entity player,
        const ::ActorListDestroyS2C* request,
        ::Empty* response);
    static void TravelToZone(entt::entity player,
        const ::TravelToZoneRequest* request,
        ::TravelToZoneResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            EnterScene(player,
                static_cast<const ::EnterSceneC2SRequest*>(request),
                static_cast<::EnterSceneC2SResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::EnterSceneC2SResponse*>(response));
			}
            break;
        case 1:
			{
            NotifyEnterScene(player,
                static_cast<const ::EnterSceneS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 2:
			{
            SceneInfoC2S(player,
                static_cast<const ::SceneInfoRequest*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 3:
			{
            NotifySceneInfo(player,
                static_cast<const ::SceneInfoS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 4:
			{
            NotifyActorCreate(player,
                static_cast<const ::ActorCreateS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 5:
			{
            NotifyActorDestroy(player,
                static_cast<const ::ActorDestroyS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 6:
			{
            NotifyActorListCreate(player,
                static_cast<const ::ActorListCreateS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 7:
			{
            NotifyActorListDestroy(player,
                static_cast<const ::ActorListDestroyS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 8:
			{
            TravelToZone(player,
                static_cast<const ::TravelToZoneRequest*>(request),
                static_cast<::TravelToZoneResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::TravelToZoneResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
