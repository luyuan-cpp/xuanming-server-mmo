#pragma once

#include <entt/src/entt/entity/entity.hpp>

// 脱战时间(秒):一对实体这么久没有再交手(一次伤害或一跳治疗),交手关系结束。
// 就是知乎原文说的 lazy time:交手对象不会因为一两秒没打就从关注列表里掉出去。
constexpr double kCombatDisengageSeconds = 10.0;

// 交手关系:谁正在和谁交手(我打的、打我的、我治疗的、治疗我的),以及脱战计时。
//
// 交手期间双方互相显式关注(AoiFollowKind::kCombat,权重 AoiPriority::kCombat,除 kPinned 外最高):
// 离得再远也互相看得见、收得到对方的移动与技能;关注表满时先挤掉别的。脱战或死亡时撤销这份关注,
// 对方仍在附近时照旧按空间来源显示。兴趣随时变靠这份来源有寿命,不靠切换权重表。
// 设计见 docs/design/aoi-directed-interest.md「战斗关注」。
//
// 只在所属场景线程上调用。复杂度只与在交手的实体及其交手对象数有关。
class CombatEngagementSystem
{
public:
    // 记一次交手:双方登记交手关系,脱战计时重新从 kCombatDisengageSeconds 开始。
    // 自己对自己、任一方无效、两者不在同一场景时忽略。
    static void RecordInteraction(entt::entity source, entt::entity target);

    // 每个模拟步调用:扣脱战计时,到时的交手关系结束;对方已无效或已不在同一场景的立即结束
    // (离场时 AoiSystem::OnLeaveScene 已清掉关注,这里只清记录)。
    static void Update(double delta);

    // 实体死亡:它的全部交手关系立即结束,双方撤销战斗关注。
    static void OnDeath(entt::entity entity);
};
