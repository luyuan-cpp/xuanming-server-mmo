#pragma once

#include <cstdint>
#include "entt/src/entt/entity/entity.hpp"

// 玩家任务写入口。所有调用都在 scene EventLoop 上；不接受客户端自报进度。
// 接取与领奖仅开放正式 scope 0，其他历史 scope 只保留存档。
class PlayerMissionSystem {
public:
    static uint32_t Initialize(entt::entity player);
    static uint32_t CheckAccept(entt::entity player, uint32_t scope, uint32_t missionId, uint64_t nowMs);
    static uint32_t Accept(entt::entity player, uint32_t scope, uint32_t missionId, uint64_t nowMs);
    // 只检查领取资格，不占用号段、不修改背包或领取状态。
    static uint32_t CheckClaim(entt::entity player, uint32_t scope, uint32_t missionId);
    static uint32_t ClaimReward(entt::entity player, uint32_t scope, uint32_t missionId);
};
