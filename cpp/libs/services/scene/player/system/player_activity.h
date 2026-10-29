#pragma once

#include <cstdint>
#include <vector>
#include "entt/src/entt/entity/entity.hpp"
#include "proto/scene/player_activity.pb.h"
#include "table/proto/activityvitalityrule_table.pb.h"
#include "table/proto/activityvitalityreward_table.pb.h"

// scene EventLoop 单写：积分只消费已完成任务事实，领取与背包在同一玩家快照提交。
// 重放原请求承担恢复查询；durable 只依据已持久的精确领取记录，不根据保存函数返回值推定。
class PlayerActivitySystem {
public:
    static void OnMissionCompleted(entt::entity player, uint32_t scope, uint32_t missionId, uint64_t nowMs);
    static uint32_t BuildVitality(entt::entity player, uint64_t nowMs, PlayerActivityVitalitySnapshot& out);
    static uint32_t Claim(entt::entity player, uint64_t nowMs,
                          const ClaimActivityVitalityRewardRequest& request,
                          ClaimActivityVitalityRewardResponse& response);
    static uint32_t FillRewardDisplay(uint32_t rewardId, PlayerActivityInfo& out);

    // 纯周期计算；所有绝对时间/长度来自正式配置，不内置日界线或客户端时区。
    static bool ResolvePeriod(const ActivityVitalityRuleTable& rule, uint64_t nowMs,
                              uint64_t& startsAtMs, uint64_t& endsAtMs);
    // 测试以同一个配置协议注入；生产 nullptr 走正式表。调用期间不缓存表行指针。
    using RuleLookupFn = bool (*)(ActivityVitalityRuleTable&, std::vector<ActivityVitalityRewardTable>&);
    using PersistFn = bool (*)(entt::entity);
    static void SetRuleLookupForTest(RuleLookupFn fn);
    static void SetPersistForTest(PersistFn fn);
};
