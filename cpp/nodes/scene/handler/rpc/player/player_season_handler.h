#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_season.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneSeasonClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetSeasonState(entt::entity player,
        const ::GetSeasonStateRequest* request,
        ::GetSeasonStateResponse* response);
    static void EquipSeasonSeal(entt::entity player,
        const ::EquipSeasonSealRequest* request,
        ::EquipSeasonSealResponse* response);
    static void CatchupSeasonSeal(entt::entity player,
        const ::CatchupSeasonSealRequest* request,
        ::CatchupSeasonSealResponse* response);
    static void EquipWuxingFormation(entt::entity player,
        const ::EquipWuxingFormationRequest* request,
        ::EquipWuxingFormationResponse* response);
    static void GetWuxingTowerState(entt::entity player,
        const ::GetWuxingTowerStateRequest* request,
        ::GetWuxingTowerStateResponse* response);
    static void StartWuxingTowerRun(entt::entity player,
        const ::StartWuxingTowerRunRequest* request,
        ::StartWuxingTowerRunResponse* response);
    static void PickWuxingTowerSigil(entt::entity player,
        const ::PickWuxingTowerSigilRequest* request,
        ::PickWuxingTowerSigilResponse* response);
    static void AbandonWuxingTowerRun(entt::entity player,
        const ::AbandonWuxingTowerRunRequest* request,
        ::AbandonWuxingTowerRunResponse* response);
    static void SaveBattleBuildBookmark(entt::entity player,
        const ::SaveBattleBuildBookmarkRequest* request,
        ::SaveBattleBuildBookmarkResponse* response);
    static void DeleteBattleBuildBookmark(entt::entity player,
        const ::DeleteBattleBuildBookmarkRequest* request,
        ::DeleteBattleBuildBookmarkResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetSeasonStateResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetSeasonState(player,
                static_cast<const ::GetSeasonStateRequest*>(request),
                static_cast<::GetSeasonStateResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipSeasonSealResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipSeasonSeal(player,
                static_cast<const ::EquipSeasonSealRequest*>(request),
                static_cast<::EquipSeasonSealResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CatchupSeasonSealResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CatchupSeasonSeal(player,
                static_cast<const ::CatchupSeasonSealRequest*>(request),
                static_cast<::CatchupSeasonSealResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipWuxingFormationResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipWuxingFormation(player,
                static_cast<const ::EquipWuxingFormationRequest*>(request),
                static_cast<::EquipWuxingFormationResponse*>(response));
			}
            break;
        case 4:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetWuxingTowerStateResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetWuxingTowerState(player,
                static_cast<const ::GetWuxingTowerStateRequest*>(request),
                static_cast<::GetWuxingTowerStateResponse*>(response));
			}
            break;
        case 5:
			{
            static_assert(std::is_same_v<decltype(std::declval<::StartWuxingTowerRunResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            StartWuxingTowerRun(player,
                static_cast<const ::StartWuxingTowerRunRequest*>(request),
                static_cast<::StartWuxingTowerRunResponse*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::PickWuxingTowerSigilResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            PickWuxingTowerSigil(player,
                static_cast<const ::PickWuxingTowerSigilRequest*>(request),
                static_cast<::PickWuxingTowerSigilResponse*>(response));
			}
            break;
        case 7:
			{
            static_assert(std::is_same_v<decltype(std::declval<::AbandonWuxingTowerRunResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            AbandonWuxingTowerRun(player,
                static_cast<const ::AbandonWuxingTowerRunRequest*>(request),
                static_cast<::AbandonWuxingTowerRunResponse*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SaveBattleBuildBookmarkResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SaveBattleBuildBookmark(player,
                static_cast<const ::SaveBattleBuildBookmarkRequest*>(request),
                static_cast<::SaveBattleBuildBookmarkResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::DeleteBattleBuildBookmarkResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            DeleteBattleBuildBookmark(player,
                static_cast<const ::DeleteBattleBuildBookmarkRequest*>(request),
                static_cast<::DeleteBattleBuildBookmarkResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
