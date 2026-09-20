#pragma once
#include <cstdint>
#include <string>

// Returns the zone-specific Kafka topic for DB tasks: "db_task_zone_{zoneId}"
inline std::string GetDbTaskTopic(uint32_t zoneId)
{
    return "db_task_zone_" + std::to_string(zoneId);
}