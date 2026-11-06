#pragma once
#include "entt/src/entt/entity/registry.hpp"
#include "proto/common/database/player_cache.pb.h"
// Unmarshal 是持久化组件的唯一创建点(emplace);Marshal 只读(try_get),不改变实体的组件集合。
// Marshal 返回 false = 有持久化组件缺失(加载不变量被破坏),此时不写出任何零值。
void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message);
[[nodiscard]] bool PlayerDatabaseMessageFieldsMarshal(entt::entity player, player_database& message);

// 全部子表成功后才替换输出；返回 false 时原输出的内容、presence 和未知字段都保持原样。
// 调用方仍须拒绝此次 Redis / DB / 快照写入，不得把拒绝当作已经落盘。
[[nodiscard]] inline bool PlayerAllDataMessageFieldsMarshal(entt::entity player, PlayerAllData& message)
{
    PlayerAllData candidate(message);
    if (!PlayerDatabaseMessageFieldsMarshal(player, *candidate.mutable_player_database_data()))
        return false;
    // 兼容旧跨服/回滚读者；数据库记录中的两项是持久化事实源。
    candidate.mutable_bag_data()->CopyFrom(candidate.player_database_data().bag_component());
    candidate.mutable_quest_data()->CopyFrom(candidate.player_database_data().mission_component());
    message.Swap(&candidate);
    return true;
}

inline void PlayerAllDataMessageFieldsUnmarshal(entt::entity player, const PlayerAllData& message)
{
    // 先把旧顶层数据并入数据库记录，再统一解码，保证派生属性初始化能看到装备与任务。
    const bool legacyBag = !message.player_database_data().has_bag_component() && message.has_bag_data();
    const bool legacyQuest = !message.player_database_data().has_mission_component() && message.has_quest_data();
    PlayerAllData upgraded;
    const PlayerAllData* source = &message;
    if (legacyBag || legacyQuest)
    {
        upgraded = message;
        auto* db = upgraded.mutable_player_database_data();
        if (legacyBag) *db->mutable_bag_component() = message.bag_data();
        if (legacyQuest) *db->mutable_mission_component() = message.quest_data();
        source = &upgraded;
    }
    PlayerDatabaseMessageFieldsUnmarshal(player, source->player_database_data());
}
