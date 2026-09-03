#pragma once

#include "proto/scene/player_attribute.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneAttributeClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetAttributePanel(entt::entity player,
        const ::GetAttributePanelRequest* request,
        ::GetAttributePanelResponse* response);
    static void AllocateAttributePoints(entt::entity player,
        const ::AllocateAttributePointsRequest* request,
        ::AllocateAttributePointsResponse* response);
    static void ResetAttributePoints(entt::entity player,
        const ::ResetAttributePointsRequest* request,
        ::ResetAttributePointsResponse* response);
    static void AutoAllocateAttributePoints(entt::entity player,
        const ::AutoAllocateAttributePointsRequest* request,
        ::AutoAllocateAttributePointsResponse* response);
    static void CreateAttributeScheme(entt::entity player,
        const ::CreateAttributeSchemeRequest* request,
        ::CreateAttributeSchemeResponse* response);
    static void SwitchAttributeScheme(entt::entity player,
        const ::SwitchAttributeSchemeRequest* request,
        ::SwitchAttributeSchemeResponse* response);
    static void RenameAttributeScheme(entt::entity player,
        const ::RenameAttributeSchemeRequest* request,
        ::RenameAttributeSchemeResponse* response);
    static void NotifyAttributePanelChanged(entt::entity player,
        const ::AttributePanelChangedS2C* request,
        ::Empty* response);
    static void GmSetPlayerLevel(entt::entity player,
        const ::GmSetPlayerLevelRequest* request,
        ::GmSetPlayerLevelResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            GetAttributePanel(player,
                static_cast<const ::GetAttributePanelRequest*>(request),
                static_cast<::GetAttributePanelResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GetAttributePanelResponse*>(response));
			}
            break;
        case 1:
			{
            AllocateAttributePoints(player,
                static_cast<const ::AllocateAttributePointsRequest*>(request),
                static_cast<::AllocateAttributePointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::AllocateAttributePointsResponse*>(response));
			}
            break;
        case 2:
			{
            ResetAttributePoints(player,
                static_cast<const ::ResetAttributePointsRequest*>(request),
                static_cast<::ResetAttributePointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::ResetAttributePointsResponse*>(response));
			}
            break;
        case 3:
			{
            AutoAllocateAttributePoints(player,
                static_cast<const ::AutoAllocateAttributePointsRequest*>(request),
                static_cast<::AutoAllocateAttributePointsResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::AutoAllocateAttributePointsResponse*>(response));
			}
            break;
        case 4:
			{
            CreateAttributeScheme(player,
                static_cast<const ::CreateAttributeSchemeRequest*>(request),
                static_cast<::CreateAttributeSchemeResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::CreateAttributeSchemeResponse*>(response));
			}
            break;
        case 5:
			{
            SwitchAttributeScheme(player,
                static_cast<const ::SwitchAttributeSchemeRequest*>(request),
                static_cast<::SwitchAttributeSchemeResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::SwitchAttributeSchemeResponse*>(response));
			}
            break;
        case 6:
			{
            RenameAttributeScheme(player,
                static_cast<const ::RenameAttributeSchemeRequest*>(request),
                static_cast<::RenameAttributeSchemeResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::RenameAttributeSchemeResponse*>(response));
			}
            break;
        case 7:
			{
            NotifyAttributePanelChanged(player,
                static_cast<const ::AttributePanelChangedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 8:
			{
            GmSetPlayerLevel(player,
                static_cast<const ::GmSetPlayerLevelRequest*>(request),
                static_cast<::GmSetPlayerLevelResponse*>(response));
            TRANSFER_ERROR_MESSAGE(static_cast<::GmSetPlayerLevelResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
