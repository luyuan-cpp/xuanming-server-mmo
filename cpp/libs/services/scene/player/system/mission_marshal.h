#pragma once

#include "entt/src/entt/entity/entity.hpp"
class QuestAllData;

namespace mission_marshal {
// 只复制状态，不触发任务接受、完成或奖励事件；用于存档、重登和快照还原。
void Marshal(entt::entity player, QuestAllData& out);
void Unmarshal(entt::entity player, const QuestAllData& in);
}
