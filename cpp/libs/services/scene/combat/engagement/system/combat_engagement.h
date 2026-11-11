#pragma once

#include <cstddef>

#include <entt/src/entt/entity/entity.hpp>

// 脱战时间(秒):一对实体这么久没有再交手,这份交手关系结束。
// 就是知乎原文说的 lazy time:交手对象不会因为一两秒没打就从关注列表里掉出去。
constexpr double kCombatDisengageSeconds = 10.0;

// 一个 NPC 的仇恨名单里最多几人互相算友方交手(一个团队副本的人数)。
// 名单里两两互相关注,开销 O(K²) 有界;更多人的混战靠空间关注,每个人依旧看得见自己打的怪。
constexpr std::size_t kMaxCombatAllyRoster = 40;

// 交手关系:谁正在和谁交手,以及脱战计时。三种来路都算交手:
//   - 敌对:我打的、打我的(RecordDamage);
//   - 治疗:我治疗的、治疗我的(RecordHeal);
//   - 一起打同一个怪:同在一个 NPC 的仇恨名单里(CombatAllyRosterComp)。
//     只对 NPC 推友方;玩家之间的混战里,打同一个人的两个人可能彼此也是敌人,不推。
//
// 交手期间双方互相显式关注(AoiFollowKind::kCombat,权重 AoiPriority::kCombat,除 kPinned 外最高):
// 离得再远也互相看得见、收得到对方的移动与技能;关注表满时先挤掉别的。脱战或死亡时撤销这份关注,
// 对方仍在附近时照旧按空间来源显示。兴趣随时变靠这份来源有寿命,不靠切换权重表。
// 设计见 docs/design/aoi-directed-interest.md「战斗关注」。
//
// 只在所属场景线程上调用。复杂度只与在交手的实体、它们的交手对象数和仇恨名单人数有关。
class CombatEngagementSystem
{
public:
    // 一次伤害(attacker 打了 target,target 没死):双方登记敌对交手并续期。
    // 一方是 NPC、另一方不是时,不是 NPC 的一方进(或留在)这个 NPC 的仇恨名单,
    // 并与名单里的其他人续期友方交手。自己打自己、任一方无效、不在同一场景时忽略。
    static void RecordDamage(entt::entity attacker, entt::entity target);

    // 一跳真回了血的治疗(healer 治了 target):双方登记交手并续期。
    // 治疗仇恨:target 正在敌对交手的每个 NPC,healer 也和它敌对交手、进它的仇恨名单。
    // 自己治自己、任一方无效、不在同一场景时忽略。
    static void RecordHeal(entt::entity healer, entt::entity target);

    // 每个模拟步调用:扣脱战计时,到时的交手关系结束;对方已无效或已不在同一场景的立即结束
    // (离场时 AoiSystem::OnLeaveScene 已清掉关注,这里只清记录与仇恨名单)。
    static void Update(double delta);

    // 实体死亡:它的全部交手关系立即结束,双方撤销战斗关注;死的是 NPC 时仇恨名单随之清空。
    // 名单里其他人之间的友方交手不在这里撤销:他们刚一起打完,按脱战时间自然结束。
    static void OnDeath(entt::entity entity);
};
