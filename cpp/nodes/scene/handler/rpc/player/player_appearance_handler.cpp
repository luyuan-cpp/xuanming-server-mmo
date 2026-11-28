
#include "player_appearance_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_appearance.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneAppearanceClientPlayerHandler::GetAppearanceInfo(entt::entity player,const ::GetAppearanceInfoRequest* request,
	::GetAppearanceInfoResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerAppearanceSystem::FillInfo(player, TimeSystem::NowSecondsUTC(), *response->mutable_info());
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::SwitchAppearance(entt::entity player,const ::SwitchAppearanceRequest* request,
	::SwitchAppearanceResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::SwitchAppearance(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::StartAppearanceTrial(entt::entity player,const ::StartAppearanceTrialRequest* request,
	::StartAppearanceTrialResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::StartTrial(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::CancelAppearanceTrial(entt::entity player,const ::CancelAppearanceTrialRequest* request,
	::CancelAppearanceTrialResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::CancelTrial(player, TimeSystem::NowSecondsUTC(), *response->mutable_info());
		err != kSuccess)
	{
		response->clear_info();
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::NotifyActorAppearanceChanged(entt::entity player,const ::ActorAppearanceChangedS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::NotifyAppearanceState(entt::entity player,const ::AppearanceStateNotify* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::EquipPetCosmetic(entt::entity player,const ::EquipPetCosmeticRequest* request,
	::EquipPetCosmeticResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::EquipPetCosmetic(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::EquipSkillVfx(entt::entity player,const ::EquipSkillVfxRequest* request,
	::EquipSkillVfxResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::EquipSkillVfx(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::BuyAppearanceSeriesBundle(entt::entity player,const ::BuyAppearanceSeriesBundleRequest* request,
	::BuyAppearanceSeriesBundleResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::BuySeriesBundle(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::EquipShowcaseCosmetic(entt::entity player,const ::EquipShowcaseCosmeticRequest* request,
	::EquipShowcaseCosmeticResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::EquipShowcaseCosmetic(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::CastCityCelebration(entt::entity player,const ::CastCityCelebrationRequest* request,
	::CastCityCelebrationResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::CastCityCelebration(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::ClaimCityCelebrationGift(entt::entity player,const ::ClaimCityCelebrationGiftRequest* request,
	::ClaimCityCelebrationGiftResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::ClaimCityCelebrationGift(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::NotifyCityCelebration(entt::entity player,const ::CityCelebrationNotify* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneAppearanceClientPlayerHandler::SendCosmeticGift(entt::entity player,const ::SendCosmeticGiftRequest* request,
	::SendCosmeticGiftResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (const auto err = PlayerAppearanceSystem::SendCosmeticGift(player, *request, TimeSystem::NowSecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
