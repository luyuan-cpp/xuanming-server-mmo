#include "kafka.h"
#include "player/constants/player.h"
#include <muduo/base/Logging.h>
#include "player/system/player_lifecycle.h"
#include "proto/common/event/player_migration_event.pb.h"

// Subscription wiring: cpp/nodes/scene/main.cpp registers BOTH topics with a
// per-node-id consumer group (`scene-cross-zone-{nodeId}`) — option A of the
// 2026-05-16 note that used to live here. Per-node groups mean EVERY scene
// node receives EVERY message on these topics; target filtering happens
// inside HandlePlayerMigration (zone check + scene-ownership check) and via
// HandlePlayerMigrationAck's frozen-entity guards. Do NOT remove those
// filters — without them one migration fans out into ghost entities on
// every node in the cluster.
void KafkaSystem::KafkaMessageHandler(const std::string& topic, const std::string& message)
{
	if (topic == kPlayerMigrateEventName)
	{
		PlayerMigrationEvent serverEvent;
		if (serverEvent.ParseFromString(message))
		{
			PlayerLifecycleSystem::HandlePlayerMigration(serverEvent);
		}
		else
		{
			LOG_ERROR << "Failed to parse ServerEvent from message: " << message;
		}
	}
	else if (topic == "player_migrate_ack")
	{
		// Destination zone confirms it loaded the migrating player. Source-side
		// scene node clears PlayerFrozenComp and finally DestroyPlayer's the
		// source entity. See docs/design/cross-zone-readiness-audit.md §3.2
		// 件 2-3 for the full ACK protocol and §7 for failure scenarios.
		//
		// Payload is `PlayerMigrationAckEvent` (protobuf, same family as
		// PlayerMigrationEvent — keeps Kafka payload format consistent across
		// the codebase, no JSON parsing on the hot path).
		PlayerMigrationAckEvent ackEvent;
		if (!ackEvent.ParseFromString(message))
		{
			LOG_WARN << "[CrossZone] Failed to parse PlayerMigrationAckEvent (size="
					 << message.size() << ") — dropping; reaper will eventually retry.";
			return;
		}
		if (ackEvent.player_id() == 0)
		{
			LOG_WARN << "[CrossZone] PlayerMigrationAckEvent has player_id=0 — dropping.";
			return;
		}
		PlayerLifecycleSystem::HandlePlayerMigrationAck(
			static_cast<Guid>(ackEvent.player_id()),
			ackEvent.to_zone());
	}
	else
	{
		LOG_WARN << "Received unknown topic: " << topic;
	}
}

