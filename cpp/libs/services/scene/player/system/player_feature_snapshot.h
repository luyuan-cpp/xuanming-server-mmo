#pragma once

#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class BagInfo;
class GetMissionListResponse;
class GetActivityListResponse;

// 这些入口只在 scene EventLoop 上调用，读取已有组件，不在开窗时创建玩家状态。
// 返回既有 tip 表的错误码；成功才发布完整快照，客户端整体覆盖。
class PlayerBagSystem {
public:
    static uint32_t BuildSnapshot(entt::entity player, uint32_t bagType, BagInfo& out);
    static uint32_t Sort(entt::entity player, uint32_t bagType, BagInfo& out, bool& changed);
};

class PlayerMissionReadSystem {
public:
    // 任务领奖/存档链路尚未完成，因此只读目录和运行态，不开放写能力。
    static uint32_t BuildList(entt::entity player, GetMissionListResponse& out);
};

class PlayerActivityReadSystem {
public:
    // 活动目录来自现有任务表；无运营排期时保持未开放。时间由调用者显式传入。
    static uint32_t BuildList(entt::entity player, uint64_t serverTimeMs, GetActivityListResponse& out);
};
