#pragma once

#include "entt/src/entt/entity/entity.hpp"
class QuestAllData;

namespace mission_marshal {
// 只复制状态，不触发任务接受、完成或奖励事件；用于存档、重登和快照还原。
// Marshal 只读,不补建 MissionsContainerComp;缺组件时输出的是没有任何 scope 的
// 「权威空记录」,落盘即清空任务。存盘入口 PlayerDatabaseMessageFieldsMarshal 会先确认组件在场。
void Marshal(entt::entity player, QuestAllData& out);
void Unmarshal(entt::entity player, const QuestAllData& in);
}
