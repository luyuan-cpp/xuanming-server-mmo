#pragma once

// 活动对局结果的纯函数(帮会同道历练,docs/design/guild-phase2/06-activities.md §6.19–§6.21)。
//
// 通道契约(跨语言;Go 侧常量在 go/guild/internal/data/trial_result_record.go 的 activityResultKey,
// 两边改一处必须同改另一处,并同步本文件的单测 battle_result_activity_test.cpp):
//   键   battle:activity_result:{battle_id}(十进制)
//   值   BattleResultEvent 序列化字节(含 activity_context)
//   存储 battle 的 zone Redis(tlsRedis.GetZoneRedis(),即 R07 battle:settlement:pending:* 所在实例;
//        文档里称 SharedRedis),必须与 guild 的 PlayerLocatorRedis 同实例、同 DB
//   TTL  7 天(与 Kafka match-results 保留期一致)
//   写入 battle,只在 activity_context.kind != NONE 时写;写成功后才发 Kafka
//   销账 guild:该局进入任何终态后 DEL
//   重发 battle 每 10s EXISTS 一次,仍在就按原字节重发 Kafka,最多 30 次;用尽后记录留给 guild 巡检器
//
// 这个头只放判定与组装:零 Redis / Kafka / muduo 依赖,单测直接盯(与 settlement/settlement_outbox.h
// 拆出来的理由相同)。IO 编排在 cpp/nodes/battle/logic/battle_room_manager.cpp。

#include <algorithm>
#include <cstdint>
#include <string>
#include <vector>

#include "proto/battle/battle_data.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

namespace turnbattle
{
    // 持久记录键前缀;完整键 = 前缀 + 十进制 battle_id(见 ActivityResultKey)。
    inline constexpr char kActivityResultKeyPrefix[] = "battle:activity_result:";
    // 7 天:guild 停机超过 7 天的胜场不补发(契约偏差 12),Kafka 保留期同值。
    inline constexpr uint32_t kActivityResultTtlSec = 7 * 24 * 3600;
    inline constexpr uint32_t kActivityResultRetryIntervalSec = 10;
    // 重发窗口 10s × 30 ≈ 5 分钟。用尽不等于丢失:记录仍在 Redis(TTL 7 天),
    // guild 巡检器(§6.33)扫超期未结算的局时直接 GET 这条记录结算。
    inline constexpr uint32_t kActivityResultRetryMaxAttempts = 30;

    inline std::string ActivityResultKey(const uint64_t battleId)
    {
        return std::string(kActivityResultKeyPrefix) + std::to_string(battleId);
    }

    // 是否活动对局。proto3 开放枚举:未知的 kind 值也按"活动对局"处理 —— 宁可多落一次库,
    // 也不能把一局真实的活动结果当普通对局只发一次。
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
    //   - kind == NONE:不写 activity_context(普通对局,结果事件照旧只发一次);
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

    enum class ActivityResultRetryAction : uint8_t
    {
        // 记录已不在(guild 已销账,或 TTL 过期):投递完成,摘掉内存条目。
        kDone,
        // 记录仍在、次数未用尽:按原字节重发 Kafka。
        kResend,
        // 次数用尽:摘掉内存条目并打响亮日志;Redis 记录保留,由 guild 巡检器兜底结算。
        kExhausted,
    };

    // 判定顺序不能换:已销账 > 次数用尽 > 重发。
    // 「已销账」排在「用尽」之前:最后一轮恰好读到已销账时算成功收尾,不能打"未送达"的假警报。
    inline ActivityResultRetryAction ClassifyActivityResultRetry(const bool recordExists,
                                                                 const uint32_t attemptsSoFar,
                                                                 const uint32_t maxAttempts)
    {
        if (!recordExists)
        {
            return ActivityResultRetryAction::kDone;
        }
        if (attemptsSoFar >= maxAttempts)
        {
            return ActivityResultRetryAction::kExhausted;
        }
        return ActivityResultRetryAction::kResend;
    }
} // namespace turnbattle
