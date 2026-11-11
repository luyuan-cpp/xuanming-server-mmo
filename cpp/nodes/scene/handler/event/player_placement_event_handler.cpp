#include "player_placement_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "player/system/player_lifecycle.h"
///<<< END WRITING YOUR CODE
void PlayerPlacementEventHandler::PlayerHostedPlacementEventHandler(const PlayerHostedPlacementEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // scene_manager --Kafka SceneCommand{DispatchEvent}--> DispatchSceneKafkaCommand 白名单解包后直接调用此 handler(loop 线程)。
    // 托管落点已在 scene_manager 提交(broker ACK 即提交点):把这名没有客户端的玩家放进本节点的 scene_id
    // (docs/design/player-connection-lost-hold.md §4.2)。gate 路由在"没有 gate 会话"时的等价物,入场链与 PlayerEnterGameNode 相同。
    // 防僵尸(target_instance_id)由共享消费者在派发前按本进程实例号过滤。
    PlayerLifecycleSystem::HostedPlacement placement;
    placement.playerId = event.player_id();
    placement.sceneId = event.scene_id();
    placement.homeZoneId = event.home_zone_id();
    placement.ownerEpoch = event.owner_epoch();
    placement.sessionId = event.session_id();
    placement.loginGeneration = event.login_generation();
    placement.ownerRevision = event.owner_revision();
    PlayerLifecycleSystem::HandleHostedPlacement(placement);
///<<< END WRITING YOUR CODE
}
