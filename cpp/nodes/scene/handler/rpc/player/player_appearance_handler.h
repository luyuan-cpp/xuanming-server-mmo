#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_appearance.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneAppearanceClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetAppearanceInfo(entt::entity player,
        const ::GetAppearanceInfoRequest* request,
        ::GetAppearanceInfoResponse* response);
    static void SwitchAppearance(entt::entity player,
        const ::SwitchAppearanceRequest* request,
        ::SwitchAppearanceResponse* response);
    static void StartAppearanceTrial(entt::entity player,
        const ::StartAppearanceTrialRequest* request,
        ::StartAppearanceTrialResponse* response);
    static void CancelAppearanceTrial(entt::entity player,
        const ::CancelAppearanceTrialRequest* request,
        ::CancelAppearanceTrialResponse* response);
    static void NotifyActorAppearanceChanged(entt::entity player,
        const ::ActorAppearanceChangedS2C* request,
        ::Empty* response);
    static void NotifyAppearanceState(entt::entity player,
        const ::AppearanceStateNotify* request,
        ::Empty* response);
    static void EquipPetCosmetic(entt::entity player,
        const ::EquipPetCosmeticRequest* request,
        ::EquipPetCosmeticResponse* response);
    static void EquipSkillVfx(entt::entity player,
        const ::EquipSkillVfxRequest* request,
        ::EquipSkillVfxResponse* response);
    static void BuyAppearanceSeriesBundle(entt::entity player,
        const ::BuyAppearanceSeriesBundleRequest* request,
        ::BuyAppearanceSeriesBundleResponse* response);
    static void EquipShowcaseCosmetic(entt::entity player,
        const ::EquipShowcaseCosmeticRequest* request,
        ::EquipShowcaseCosmeticResponse* response);
    static void CastCityCelebration(entt::entity player,
        const ::CastCityCelebrationRequest* request,
        ::CastCityCelebrationResponse* response);
    static void ClaimCityCelebrationGift(entt::entity player,
        const ::ClaimCityCelebrationGiftRequest* request,
        ::ClaimCityCelebrationGiftResponse* response);
    static void NotifyCityCelebration(entt::entity player,
        const ::CityCelebrationNotify* request,
        ::Empty* response);
    static void SendCosmeticGift(entt::entity player,
        const ::SendCosmeticGiftRequest* request,
        ::SendCosmeticGiftResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetAppearanceInfoResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetAppearanceInfo(player,
                static_cast<const ::GetAppearanceInfoRequest*>(request),
                static_cast<::GetAppearanceInfoResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SwitchAppearanceResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SwitchAppearance(player,
                static_cast<const ::SwitchAppearanceRequest*>(request),
                static_cast<::SwitchAppearanceResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::StartAppearanceTrialResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            StartAppearanceTrial(player,
                static_cast<const ::StartAppearanceTrialRequest*>(request),
                static_cast<::StartAppearanceTrialResponse*>(response));
			}
            break;
        case 3:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CancelAppearanceTrialResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CancelAppearanceTrial(player,
                static_cast<const ::CancelAppearanceTrialRequest*>(request),
                static_cast<::CancelAppearanceTrialResponse*>(response));
			}
            break;
        case 4:
			{
            NotifyActorAppearanceChanged(player,
                static_cast<const ::ActorAppearanceChangedS2C*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 5:
			{
            NotifyAppearanceState(player,
                static_cast<const ::AppearanceStateNotify*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 6:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipPetCosmeticResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipPetCosmetic(player,
                static_cast<const ::EquipPetCosmeticRequest*>(request),
                static_cast<::EquipPetCosmeticResponse*>(response));
			}
            break;
        case 7:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipSkillVfxResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipSkillVfx(player,
                static_cast<const ::EquipSkillVfxRequest*>(request),
                static_cast<::EquipSkillVfxResponse*>(response));
			}
            break;
        case 8:
			{
            static_assert(std::is_same_v<decltype(std::declval<::BuyAppearanceSeriesBundleResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            BuyAppearanceSeriesBundle(player,
                static_cast<const ::BuyAppearanceSeriesBundleRequest*>(request),
                static_cast<::BuyAppearanceSeriesBundleResponse*>(response));
			}
            break;
        case 9:
			{
            static_assert(std::is_same_v<decltype(std::declval<::EquipShowcaseCosmeticResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            EquipShowcaseCosmetic(player,
                static_cast<const ::EquipShowcaseCosmeticRequest*>(request),
                static_cast<::EquipShowcaseCosmeticResponse*>(response));
			}
            break;
        case 10:
			{
            static_assert(std::is_same_v<decltype(std::declval<::CastCityCelebrationResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            CastCityCelebration(player,
                static_cast<const ::CastCityCelebrationRequest*>(request),
                static_cast<::CastCityCelebrationResponse*>(response));
			}
            break;
        case 11:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimCityCelebrationGiftResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimCityCelebrationGift(player,
                static_cast<const ::ClaimCityCelebrationGiftRequest*>(request),
                static_cast<::ClaimCityCelebrationGiftResponse*>(response));
			}
            break;
        case 12:
			{
            NotifyCityCelebration(player,
                static_cast<const ::CityCelebrationNotify*>(request),
                static_cast<::Empty*>(response));
			}
            break;
        case 13:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SendCosmeticGiftResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SendCosmeticGift(player,
                static_cast<const ::SendCosmeticGiftRequest*>(request),
                static_cast<::SendCosmeticGiftResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
