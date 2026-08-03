#pragma once
#include <cstdint>
#include <string>
constexpr char kPlayerMigrateEventName[] = "player_migrate";

// Returns the zone-specific Kafka topic for DB tasks: "db_task_zone_{zoneId}"
inline std::string GetDbTaskTopic(uint32_t zoneId)
{
    return "db_task_zone_" + std::to_string(zoneId);
}