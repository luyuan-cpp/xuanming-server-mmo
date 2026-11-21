#include "actor_state_attribute_sync.h"

#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "player/comp/player_frozen_comp.h"  // cross-zone-readiness-audit.md §11.2
#include "proto/common/component/player_network_comp.pb.h"
#include "engine/core/type_define/type_define.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/system/interest.h"  // GetVisibleWatchers:属性同步的接收者
#include "rpc/service_metadata/player_state_attribute_sync_service_metadata.h"

#include "frame/manager/frame_time.h"
#include "network/client_message_sender.h"
#include <generated/attribute/actorbaseattributess2c_attribute_sync.h>
#include <thread_context/ecs_context.h>

void ActorStateAttributeSyncSystem::Update(const double)
{
	const auto currentFrame = tlsFrameTimeManager.frameTime.current_frame();
	// R20 降载：基础属性广播每 2 帧跑一次，且仅遍历有脏标记的实体，避免无脏实体的全表空转与死增量分桶。
	if (currentFrame % 2 != 0)
	{
		return;
	}

	EntityVector visibleWatchers;
	EntityVector playerWatchers;

	// PlayerFrozenComp exclude: a frozen player's broadcast position /
	// attribute snapshot was already captured into PlayerAllData.
	// 仅向有 PlayerSessionSnapshotComp 的玩家观察者广播，过滤 NPC 观察者，避免每 2 帧刷 LOG_ERROR。
	for (auto [entity, dirtyMask, watchersComp] :
		tlsEcs.actorRegistry.view<ActorBaseAttributesS2CDirtyMaskComp, AoiWatchersComp>(
			entt::exclude<PlayerFrozenComp>).each())
	{
		if (watchersComp.watchers.empty()) continue;

		visibleWatchers.clear();
		InterestSystem::GetVisibleWatchers(entity, visibleWatchers);
		if (visibleWatchers.empty()) continue;

		playerWatchers.clear();
		for (const auto watcher : visibleWatchers)
		{
			if (const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(watcher);
				session != nullptr && session->gate_session_id() != kInvalidSessionId)
			{
				playerWatchers.emplace_back(watcher);
			}
		}
		if (playerWatchers.empty()) continue;

		ActorBaseAttributesS2CSyncAttributes(entity, ScenePlayerSyncSyncBaseAttributeMessageId, playerWatchers);
	}
}

