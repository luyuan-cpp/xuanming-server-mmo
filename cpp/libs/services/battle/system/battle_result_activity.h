#pragma once

// 对局结果里活动上下文与逃跑 / 阵亡名单的组装(帮会同道历练,docs/design/guild-phase2/06-activities.md §6.19–§6.21)。
//
// 这个头只放组装:零 Redis / Kafka / muduo 依赖,单测直接盯(battle_result_activity_test.cpp)。
// 对局结果随终局原件提交 match SQL(CommitBattleOutcome),活动局结果由 match 发件箱投 Kafka,
// battle 不写 Redis、不直接发 Kafka(player-data-tidb-scaling-plan.md §5.4)。

#include <algorithm>
#include <cstdint>
#include <vector>

#include "proto/battle/battle_data.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

namespace turnbattle
{
    // 是否活动对局。proto3 开放枚举:未知的 kind 值也按"活动对局"处理 —— 宁可多回显一次上下文,
    // 也不能把一局真实的活动结果当普通对局。
    inline bool IsActivityBattle(const ::BattleActivityContext &activity)
    {
        return activity.kind() != ::BATTLE_ACTIVITY_KIND_NONE;
    }

    // 玩家 id 名单升序去重(结果事件里的 fled / dead 名单契约)。
    inline void SortUniquePlayerIds(std::vector<uint64_t> &ids)
    {
        std::sort(ids.begin(), ids.end());
        ids.erase(std::unique(ids.begin(), ids.end()), ids.end());
    }

    // 把开局时的活动上下文、逃跑与阵亡名单写进对局结果。
    //   - kind == NONE:不写 activity_context(普通对局);
    //   - 两份名单所有对局都写,升序去重。
    // 用户决策 U2(阵亡也得奖):guild 发奖候选 = team 0 − fled_player_ids;dead_player_ids 照样回显,
    // 只供统计与日后回退,不参与过滤 —— 过滤口径在 guild,battle 只如实报告。
    inline void FillBattleResultActivityFields(const ::BattleActivityContext &activity,
                                               std::vector<uint64_t> fledPlayerIds,
                                               std::vector<uint64_t> deadPlayerIds,
                                               contracts::kafka::BattleResultEvent &out)
    {
        if (IsActivityBattle(activity))
        {
            *out.mutable_activity_context() = activity;
        }
        SortUniquePlayerIds(fledPlayerIds);
        SortUniquePlayerIds(deadPlayerIds);
        for (const auto id : fledPlayerIds)
        {
            out.add_fled_player_ids(id);
        }
        for (const auto id : deadPlayerIds)
        {
            out.add_dead_player_ids(id);
        }
    }
} // namespace turnbattle
