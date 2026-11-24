
#include "player_trial_journal_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "engine/core/time/system/time.h"
#include "services/scene/player/system/player_trial_journal.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneTrialJournalClientPlayerHandler::GetTrialJournal(entt::entity player,const ::GetTrialJournalRequest* request,
	::GetTrialJournalResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	(void)request;
	if (const uint32_t err = PlayerTrialJournalSystem::GetJournal(player, TimeSystem::NowMillisecondsUTC(), *response);
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneTrialJournalClientPlayerHandler::ClaimTrialJournalMilestone(entt::entity player,const ::ClaimTrialJournalMilestoneRequest* request,
	::ClaimTrialJournalMilestoneResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerTrialJournalSystem::ClaimMilestone(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}

void SceneTrialJournalClientPlayerHandler::RedeemBoundArtifactVoucher(entt::entity player,const ::RedeemBoundArtifactVoucherRequest* request,
	::RedeemBoundArtifactVoucherResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	PlayerTrialJournalSystem::RedeemArtifactVoucher(player, TimeSystem::NowMillisecondsUTC(), *request, *response);
///<<< END WRITING YOUR CODE

}
