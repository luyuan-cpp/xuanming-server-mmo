
#include "player_battle_result_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "battle/system/player_battle.h"
#include "table/proto/tip/common_error_tip.pb.h"
///<<< END WRITING YOUR CODE

void SceneBattleResultClientPlayerHandler::GetRecentBattleResults(entt::entity player,const ::GetRecentBattleResultsRequest* request,
	::GetRecentBattleResultsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 只认会话里的本人:player 由会话解析,请求不带 player_id。规则全在 PlayerBattleSystem
	// (docs/design/battle-result-recovery.md §6);这里只把失败码写进应答。
	if (const auto err = PlayerBattleSystem::GetRecentBattleResults(player, *response); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}

void SceneBattleResultClientPlayerHandler::AckBattleResults(entt::entity player,const ::AckBattleResultsRequest* request,
	::AckBattleResultsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 确认已展示:超过 10 个 battle_id 回 1015,冻结 / 交接中回 1006;不认识的局忽略,重复确认无副作用。
	if (const auto err = PlayerBattleSystem::AckBattleResults(player, *request); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
	}
///<<< END WRITING YOUR CODE

}
