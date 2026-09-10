
#include "player_bag_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
///<<< END WRITING YOUR CODE

void SceneBagClientPlayerHandler::GetBag(entt::entity player,const ::GetBagRequest* request,
	::GetBagResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto result = PlayerBagSystem::BuildSnapshot(player, request->bag_type(), *response->mutable_bag());
    if (result != kSuccess) {
        response->clear_bag();
        tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
    }
///<<< END WRITING YOUR CODE
}

void SceneBagClientPlayerHandler::SortBag(entt::entity player,const ::SortBagRequest* request,
	::SortBagResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
bool changed = false;
    const auto result = PlayerBagSystem::Sort(player, request->bag_type(), *response->mutable_bag(), changed);
    if (result != kSuccess) {
        response->clear_bag();
        tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
        return;
    }
    response->set_changed(changed);
///<<< END WRITING YOUR CODE
}
