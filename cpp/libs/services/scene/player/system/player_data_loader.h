#pragma once
#include "entt/src/entt/entity/registry.hpp"
#include "proto/common/database/player_cache.pb.h"
#include "services/scene/player/system/bag_marshal.h"
#include "services/scene/player/system/mission_marshal.h"
void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message);
void PlayerDatabaseMessageFieldsMarshal(entt::entity player, player_database& message);

void PlayerDatabase1MessageFieldsUnmarshal(entt::entity player, const player_database_1& message);
void PlayerDatabase1MessageFieldsMarshal(entt::entity player, player_database_1& message);

inline void PlayerAllDataMessageFieldsMarshal(entt::entity player, PlayerAllData& message)
{
PlayerDatabaseMessageFieldsMarshal(player, *message.mutable_player_database_data());
PlayerDatabase1MessageFieldsMarshal(player, *message.mutable_player_database_1_data());
// 兼容旧跨服/回滚读者；数据库记录中的两项是持久化事实源。
message.mutable_bag_data()->CopyFrom(message.player_database_data().bag_component());
message.mutable_quest_data()->CopyFrom(message.player_database_data().mission_component());
}

inline void PlayerAllDataMessageFieldsUnMarshal(entt::entity player, const PlayerAllData& message)
{
PlayerDatabaseMessageFieldsUnmarshal(player, message.player_database_data());
PlayerDatabase1MessageFieldsUnmarshal(player, message.player_database_1_data());
// 旧快照没有新增数据库字段时，仍恢复已有顶层数据。
if (!message.player_database_data().has_bag_component() && message.has_bag_data())
    bag_marshal::Unmarshal(player, message.bag_data());
if (!message.player_database_data().has_mission_component() && message.has_quest_data())
    mission_marshal::Unmarshal(player, message.quest_data());
}
