#include "mission_marshal.h"

#include <algorithm>
#include <set>
#include <vector>
#include "thread_context/ecs_context.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "services/scene/player/system/player_mission.h"

namespace mission_marshal {
void Marshal(entt::entity player, QuestAllData& out) {
    out.Clear();
    out.set_scoped_state_present(true);
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    if (container == nullptr) return;
    // 固定输出顺序，避免 unordered_map 迭代序让相同业务状态被误判为变更。
    std::vector<uint32_t> scopes;
    for (const auto& [scope, missions] : container->map) scopes.push_back(scope);
    std::sort(scopes.begin(), scopes.end());
    for (const auto scope : scopes) {
        const auto& missions = container->map.at(scope);
        auto* data = out.add_scopes();
        data->set_scope(scope);
        data->mutable_mission_list()->CopyFrom(missions.GetMissionList());
        data->mutable_mission_list()->set_type(scope);
        // 新存档按任务稳定 ID 保存，旧位图字符串不再写，防止生成位序被误解。
        data->mutable_mission_list()->clear_complete_missions();
        std::set<uint32_t> completed(missions.GetUnmappedCompletedIds().begin(), missions.GetUnmappedCompletedIds().end());
        std::set<uint32_t> claimable(missions.GetUnmappedClaimableIds().begin(), missions.GetUnmappedClaimableIds().end());
        for (const auto& [id, index] : MissionBitMap) {
            if (missions.IsComplete(static_cast<uint32_t>(id))) completed.insert(static_cast<uint32_t>(id));
            if (missions.IsClaimable(static_cast<uint32_t>(id))) claimable.insert(static_cast<uint32_t>(id));
        }
        for (const auto id : completed) data->add_completed_mission_ids(id);
        for (const auto id : claimable) data->add_claimable_mission_ids(id);
    }
}

void Unmarshal(entt::entity player, const QuestAllData& in) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    auto& container = tlsEcs.actorRegistry.get_or_emplace<MissionsContainerComp>(player);
    container.map.clear();
    if (in.scoped_state_present() || !in.scopes().empty()) {
        for (const auto& scope : in.scopes()) {
            // 重复 scope 合并保留状态；同 ID 的首份活动进度获胜，不重置成后续默认值。
            auto& missions = container.GetOrCreate(scope.scope());
            auto& list = missions.GetMutableMissionList();
            list.set_type(scope.scope());
            for (const auto& [id, mission] : scope.mission_list().missions())
                list.mutable_missions()->insert({id, mission});
            for (const auto& [id, time] : scope.mission_list().mission_begin_time())
                list.mutable_mission_begin_time()->insert({id, time});
            for (const auto id : scope.completed_mission_ids()) missions.RestoreCompleted(id);
            for (const auto id : scope.claimable_mission_ids()) missions.RestoreClaimable(id);
        }
    } else {
        // 兼容原占位协议：progress 只覆盖首目标，其余填 0；绝不由旧完成记录推定未领奖。
        auto& missions = container.GetOrCreate(MissionListComp::kPlayerMission);
        auto& list = missions.GetMutableMissionList();
        list.set_type(MissionListComp::kPlayerMission);
        // 旧 repeated 允许同一配置 ID 重复；显式已完成/已交付优先，不能再导入待领权。
        std::set<uint32_t> finished;
        for (const auto& entry : in.active())
            if (entry.state() == 3) finished.insert(entry.config_id());
        for (const auto& entry : in.active()) {
            if (finished.contains(entry.config_id())) {
                missions.RestoreCompleted(entry.config_id());
                missions.ClearClaimable(entry.config_id());
                continue;
            }
            if (entry.state() == 2) {
                missions.RestoreCompleted(entry.config_id());
                missions.RestoreClaimable(entry.config_id());
                list.mutable_missions()->erase(entry.config_id());
                (*list.mutable_mission_begin_time())[entry.config_id()] = entry.accepted_at_ms();
                continue;
            }
            // 首份活动进度获胜；不能把后来的副本追加成多余目标槽。
            if (missions.IsComplete(entry.config_id()) || list.missions().find(entry.config_id()) != list.missions().end()) continue;
            auto& active = (*list.mutable_missions())[entry.config_id()];
            active.set_id(entry.config_id());
            active.add_progress(entry.progress());
            const auto [row, error] = MissionTableManager::Instance().FindByIdSilent(entry.config_id());
            if (row != nullptr)
                while (active.progress_size() < row->condition_id_size()) active.add_progress(0);
            (*list.mutable_mission_begin_time())[entry.config_id()] = entry.accepted_at_ms();
        }
        for (const auto id : in.completed()) missions.RestoreCompleted(id);
    }
    for (auto& [scope, missions] : container.map) missions.RebuildIndexes(MissionConfig::GetSingleton());
    (void)PlayerMissionSystem::Initialize(player);
}
} // namespace mission_marshal
