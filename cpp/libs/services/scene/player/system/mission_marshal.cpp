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
        // 已完成按位序位图落盘,每个任务 1 位(格式见 MissionsComp::CompletedBitsForSave)。
        data->mutable_mission_list()->set_complete_missions(missions.CompletedBitsForSave());
        // 待领取的很少,存任务 ID;本二进制不认识的 ID 原样带回。
        std::set<uint32_t> claimable(missions.GetUnmappedClaimableIds().begin(), missions.GetUnmappedClaimableIds().end());
        for (const auto& [id, index] : MissionBitMap)
            if (missions.IsClaimable(static_cast<uint32_t>(id))) claimable.insert(static_cast<uint32_t>(id));
        for (const auto id : claimable) data->add_claimable_mission_ids(id);
    }
}

void Unmarshal(entt::entity player, const QuestAllData& in) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    auto& container = tlsEcs.actorRegistry.get_or_emplace<MissionsContainerComp>(player);
    container.map.clear();
    std::set<uint32_t> seenScopes;
    for (const auto& scope : in.scopes()) {
        // 重复 scope 合并保留状态；同 ID 的首份活动进度获胜，不重置成后续默认值。
        auto& missions = container.GetOrCreate(scope.scope());
        auto& list = missions.GetMutableMissionList();
        // MissionListComp 本层的未知字段(新版本加的字段)随运行时列表携带,存盘时由 Marshal 的 CopyFrom 原样带回
        // (docs/design/mixed-version-persist-field-protection.md §4.2 L3)。只取首份:与「首份获胜」同口径,
        // 重复 scope 再并一次会让重复字段翻倍。
        if (seenScopes.insert(scope.scope()).second) {
            list.mutable_unknown_fields()->MergeFrom(scope.mission_list().unknown_fields());
            if (scope.mission_list().has_activity_ledger())
                list.mutable_activity_ledger()->CopyFrom(scope.mission_list().activity_ledger());
        }
        list.set_type(scope.scope());
        for (const auto& [id, mission] : scope.mission_list().missions())
            list.mutable_missions()->insert({id, mission});
        missions.RestoreCompletedBits(scope.mission_list().complete_missions());
        for (const auto id : scope.claimable_mission_ids()) missions.RestoreClaimable(id);
    }
    for (auto& [scope, missions] : container.map) missions.RebuildIndexes(MissionConfig::GetSingleton());
    (void)PlayerMissionSystem::Initialize(player);
}
} // namespace mission_marshal
