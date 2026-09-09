#pragma once

#include "proto/scene/player_pet.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class ScenePetClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetPetList(entt::entity player,
        const ::GetPetListRequest* request,
        ::GetPetListResponse* response);
    static void SummonPet(entt::entity player,
        const ::SummonPetRequest* request,
        ::SummonPetResponse* response);
    static void RecallPet(entt::entity player,
        const ::RecallPetRequest* request,
        ::RecallPetResponse* response);
    static void AllocatePetPoints(entt::entity player,
        const ::AllocatePetPointsRequest* request,
        ::AllocatePetPointsResponse* response);
    static void ResetPetPoints(entt::entity player,
        const ::ResetPetPointsRequest* request,
        ::ResetPetPointsResponse* response);
    static void AutoAllocatePetPoints(entt::entity player,
        const ::AutoAllocatePetPointsRequest* request,
        ::AutoAllocatePetPointsResponse* response);
    static void RenamePet(entt::entity player,
        const ::RenamePetRequest* request,
        ::RenamePetResponse* response);
    static void NotifyPetListChanged(entt::entity player,
        const ::PetListChangedS2C* request,
        ::Empty* response);
    static void GmGrantPet(entt::entity player,
        const ::GmGrantPetRequest* request,
        ::GmGrantPetResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            GetPetList(player,
                static_cast<const ::GetPetListRequest*>(request),
                static_cast<::GetPetListResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GetPetListResponse*>(response));
			}
            break;
        case 1:
			{
            SummonPet(player,
                static_cast<const ::SummonPetRequest*>(request),
                static_cast<::SummonPetResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::SummonPetResponse*>(response));
			}
            break;
        case 2:
			{
            RecallPet(player,
                static_cast<const ::RecallPetRequest*>(request),
                static_cast<::RecallPetResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::RecallPetResponse*>(response));
			}
            break;
        case 3:
			{
            AllocatePetPoints(player,
                static_cast<const ::AllocatePetPointsRequest*>(request),
                static_cast<::AllocatePetPointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::AllocatePetPointsResponse*>(response));
			}
            break;
        case 4:
			{
            ResetPetPoints(player,
                static_cast<const ::ResetPetPointsRequest*>(request),
                static_cast<::ResetPetPointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::ResetPetPointsResponse*>(response));
			}
            break;
        case 5:
			{
            AutoAllocatePetPoints(player,
                static_cast<const ::AutoAllocatePetPointsRequest*>(request),
                static_cast<::AutoAllocatePetPointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::AutoAllocatePetPointsResponse*>(response));
			}
            break;
        case 6:
			{
            RenamePet(player,
                static_cast<const ::RenamePetRequest*>(request),
                static_cast<::RenamePetResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::RenamePetResponse*>(response));
			}
            break;
        case 7:
			{
            NotifyPetListChanged(player,
                static_cast<const ::PetListChangedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 8:
			{
            GmGrantPet(player,
                static_cast<const ::GmGrantPetRequest*>(request),
                static_cast<::GmGrantPetResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GmGrantPetResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
