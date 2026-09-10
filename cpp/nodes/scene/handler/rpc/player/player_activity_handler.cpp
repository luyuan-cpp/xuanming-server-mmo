
#include "player_activity_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include <muduo/base/Timestamp.h>
///<<< END WRITING YOUR CODE

void SceneActivityClientPlayerHandler::GetActivityList(entt::entity player,const ::GetActivityListRequest* request,
	::GetActivityListResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto nowMs = static_cast<uint64_t>(muduo::Timestamp::now().microSecondsSinceEpoch() / 1000);
    const auto result = PlayerActivityReadSystem::BuildList(player, nowMs, *response);
    if (result != kSuccess) tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
///<<< END WRITING YOUR CODE
}
