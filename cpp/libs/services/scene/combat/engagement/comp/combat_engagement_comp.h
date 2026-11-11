#pragma once

#include <unordered_map>

#include <entt/src/entt/entity/entity.hpp>

// 交手关系(按需状态,只由 CombatEngagementSystem 写):对方 → 距脱战还剩的秒数。
// 一对实体两边各记一份、互为镜像,同时续期、同时结束。第一次交手时创建,最后一份关系结束时摘除;
// 实体销毁自然清理。读缺失 = 没有在交手。
struct CombatEngagementComp
{
    std::unordered_map<entt::entity, double> secondsUntilDisengage;
};
