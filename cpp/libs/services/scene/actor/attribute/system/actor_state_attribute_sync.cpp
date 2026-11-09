#include "actor_state_attribute_sync.h"

#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "actor/attribute/constants/actor_state_attribute_constants.h"
#include "player/comp/player_frozen_comp.h"  // cross-zone-readiness-audit.md §11.2
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/system/interest.h"  // GetVisibleWatchers:属性同步的接收者
#include "spatial/system/view.h"
#include "rpc/service_metadata/player_state_attribute_sync_service_metadata.h"

#include "frame/manager/frame_time.h"
#include "network/client_message_sender.h"
#include <generated/attribute/actorbaseattributess2c_attribute_sync.h>
#include <generated/attribute/attributedelta60framess2c_attribute_sync.h>
#include <generated/attribute/attributedelta30framess2c_attribute_sync.h>
#include <generated/attribute/attributedelta10framess2c_attribute_sync.h>
#include <generated/attribute/attributedelta5framess2c_attribute_sync.h>
#include <generated/attribute/attributedelta2framess2c_attribute_sync.h>
#include <thread_context/ecs_context.h>

constexpr uint32_t kSyncFrequencyArraySize = 5;

using SyncFrequencyArray = std::array<uint32_t, kSyncFrequencyArraySize>;

// Sync frequencies per distance level
constexpr SyncFrequencyArray kLevel1SyncFrequencies{
	eAttributeSyncFrequency::kSyncEvery2Frames,
	eAttributeSyncFrequency::kSyncEvery5Frames,
	eAttributeSyncFrequency::kSyncEvery10Frames,
	eAttributeSyncFrequency::kSyncEvery30Frames,
	eAttributeSyncFrequency::kSyncEvery60Frames };

constexpr SyncFrequencyArray kLevel2SyncFrequencies{
	eAttributeSyncFrequency::kSyncEvery2Frames,
	eAttributeSyncFrequency::kSyncEvery5Frames,
	eAttributeSyncFrequency::kSyncEvery10Frames };

constexpr SyncFrequencyArray kLevel3SyncFrequencies{
	eAttributeSyncFrequency::kSyncEvery2Frames };

constexpr double kLevel1RadiusFactor = 0.333;
constexpr double kLevel2RadiusFactor = 0.666;

// Sync entities at the appropriate frequencies for this frame
static void SyncEntitiesAtFrequencies(entt::entity entity, const EntityVector& entities,
									  const SyncFrequencyArray& frequencies, uint32_t currentFrame)
{
	if (entities.empty()) return;
	for (const auto& frequency : frequencies) {
		if (frequency > 0 && currentFrame % frequency == 0) {
			ActorStateAttributeSyncSystem::SyncAttributes(entity, entities, frequency);
		}
	}
}

void ActorStateAttributeSyncSystem::Update(const double delta)
{
	const auto currentFrame = tlsFrameTimeManager.frameTime.current_frame();
	EntityVector allAoiEntities, level1Entities, level2Entities, level3Entities;

	// PlayerFrozenComp exclude: a frozen player's broadcast position /
	// attribute snapshot was already captured into PlayerAllData. Source-
	// side ongoing AOI broadcasts would leak stale state to neighbours
	// and confuse the destination zone's first-frame reconciliation.
	// cross-zone-readiness-audit.md §11.2.
	// 接收者 = 客户端正在显示 entity 的观察者(AOI 反向索引中可见的那些)。关注是单向的:
	// 属性跟着"谁关注并显示我"走,不是"我看得见谁"(docs/design/aoi-directed-interest.md)。
	for (auto [entity, transform, watchersComp] : tlsEcs.actorRegistry.view<Transform, AoiWatchersComp>(
			entt::exclude<PlayerFrozenComp>).each())
	{
		if (watchersComp.watchers.empty()) continue;

		// Build flat entity list for base attribute sync (avoids hash set allocation)
		allAoiEntities.clear();
		InterestSystem::GetVisibleWatchers(entity, allAoiEntities);
		if (allAoiEntities.empty()) continue;

		// R20 load shed: base-attribute broadcast was running EVERY frame to every
		// visible neighbour, one of the heaviest per-frame AOI costs under 45k
		// concentrated players. It is already dirty-gated (only emits on change),
		// so dropping to every 2nd frame delays a sync by at most one frame
		// (~33ms) while halving the per-frame target-list iteration + dispatch.
		if (currentFrame % 2 == 0) {
			ActorBaseAttributesS2CSyncAttributes(entity, ScenePlayerSyncSyncBaseAttributeMessageId, allAoiEntities);
		}

		// Single-pass distance classification: compute distance once per neighbor
		level1Entities.clear();
		level2Entities.clear();
		level3Entities.clear();

		for (const auto watcher : allAoiEntities) {
			const double maxRadius = ViewSystem::GetMaxViewRadius(watcher);
			const auto distanceOpt = ViewSystem::GetDistanceBetweenEntities(watcher, entity);
			if (!distanceOpt)
				continue;
			const double distance = *distanceOpt;

			if (distance <= maxRadius * kLevel1RadiusFactor) {
				level1Entities.emplace_back(watcher);
			} else if (distance <= maxRadius * kLevel2RadiusFactor) {
				level2Entities.emplace_back(watcher);
			} else {
				// 视野半径外的显式关注者(技能追踪等)仍在显示它:按最低档同步,不因距离整组丢掉。
				level3Entities.emplace_back(watcher);
			}
		}

		SyncEntitiesAtFrequencies(entity, level1Entities, kLevel1SyncFrequencies, currentFrame);
		SyncEntitiesAtFrequencies(entity, level2Entities, kLevel2SyncFrequencies, currentFrame);
		SyncEntitiesAtFrequencies(entity, level3Entities, kLevel3SyncFrequencies, currentFrame);
	}
}

void ActorStateAttributeSyncSystem::SyncAttributes(entt::entity entity, const EntityVector& receivingObservers, uint32_t syncFrequency) {
    if (receivingObservers.empty()) return;

    switch (syncFrequency) {
    case eAttributeSyncFrequency::kSyncEvery2Frames: {
		AttributeDelta2FramesS2CSyncAttributes(entity, ScenePlayerSyncSyncAttribute2FramesMessageId, receivingObservers);
        break;
    }
    case eAttributeSyncFrequency::kSyncEvery5Frames: {
		AttributeDelta5FramesS2CSyncAttributes(entity, ScenePlayerSyncSyncAttribute5FramesMessageId, receivingObservers);
        break;
    }
    case eAttributeSyncFrequency::kSyncEvery10Frames: {
		AttributeDelta10FramesS2CSyncAttributes(entity, ScenePlayerSyncSyncAttribute10FramesMessageId, receivingObservers);
        break;
    }
    case eAttributeSyncFrequency::kSyncEvery30Frames: {
		AttributeDelta30FramesS2CSyncAttributes(entity, ScenePlayerSyncSyncAttribute30FramesMessageId, receivingObservers);
        break;
    }
    case eAttributeSyncFrequency::kSyncEvery60Frames: {
		AttributeDelta60FramesS2CSyncAttributes(entity, ScenePlayerSyncSyncAttribute60FramesMessageId, receivingObservers);
        break;
    }
    default:
        break;
    }
}

