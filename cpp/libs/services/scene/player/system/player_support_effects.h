#pragma once
#include <entt/src/entt/entity/entity.hpp>
class SupportEffectSummary;

// scene人物辅助来源只读投影；战斗临时状态须由本人battle快照接管，不在此猜测。
class PlayerSupportEffectSystem {
public:
    static void Build(entt::entity player, SupportEffectSummary& summary);
};
