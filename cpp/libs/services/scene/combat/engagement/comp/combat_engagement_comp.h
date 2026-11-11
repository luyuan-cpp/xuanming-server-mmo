#pragma once

#include <unordered_map>
#include <vector>

#include <entt/src/entt/entity/entity.hpp>

// 一对实体之间的一份交手记录。
//   - secondsUntilDisengage:距脱战还剩的秒数,每次交手重新计时;
//   - hostile:这对实体之间有过伤害(敌对)。治疗、一起打同一个怪只算协同,不置敌对;
//     置上之后保留到这份记录结束。
struct CombatEngagementRecord
{
    double secondsUntilDisengage = 0;
    bool hostile = false;
};

// 交手关系(按需状态,只由 CombatEngagementSystem 写):对方 → 交手记录。
// 一对实体两边各记一份、互为镜像,同时续期、同时结束。第一次交手时创建,最后一份记录结束时摘除;
// 实体销毁自然清理。读缺失 = 没有在交手。
struct CombatEngagementComp
{
    std::unordered_map<entt::entity, CombatEngagementRecord> records;
};

// NPC 的仇恨名单(按需状态,只挂在 NPC 上,只由 CombatEngagementSystem 写):
// 正在和它敌对交手的非 NPC 实体,按加入先后最多 kMaxCombatAllyRoster 名。
// 名单里的人互相算友方交手(一起打这个怪)。名单满了,后来的人照样和这个 NPC 交手、看得见它,
// 只是不和名单里的人互相关注;有人离开名单后,它下一次出手时补进来。
// 成员和这个 NPC 的交手记录结束时移出;空了摘除组件,NPC 销毁时随实体清理。
// 只记成员,不记仇恨值:AI 选目标需要仇恨值时再加。
struct CombatAllyRosterComp
{
    std::vector<entt::entity> members;
};
