#include "player_activity.h"

#include <algorithm>
#include <limits>
#include <set>
#include <string>
#include <tuple>
#include <vector>
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/mission/comp/mission_comp.h"
#include "muduo/base/Logging.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "proto/common/component/battle_comp.pb.h"
#include "table/code/activityschedule_table.h"
#include "table/code/activityvitalityrule_table.h"
#include "table/code/activityvitalityreward_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/code/reward_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/reward_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr int kMaxLedgerRecords = 4096;
constexpr size_t kMaxThresholds = 64;
// 已结束周期的领取记录留多少条供重放原请求。不小于单周期档数上限,上一周期的领取全部留得住。
constexpr int kKeptPastClaims = 64;
static_assert(kKeptPastClaims >= static_cast<int>(kMaxThresholds));
constexpr uint64_t kResaveIntervalMs = 1000;
struct ActivityPersistRequestComp { uint64_t lastRequestMs{0}; };
PlayerActivitySystem::RuleLookupFn ruleLookup = nullptr;
PlayerActivitySystem::PersistFn persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;

bool LoadPolicy(ActivityVitalityRuleTable& rule, std::vector<ActivityVitalityRewardTable>& rewards) {
    rewards.clear();
    if (ruleLookup != nullptr) return ruleLookup(rule, rewards);
    bool found = false;
    for (const auto& row : ActivityVitalityRuleTableManager::Instance().FindAll().data()) {
        if (!row.enabled()) continue;
        if (found) return false;
        rule = row;
        found = true;
    }
    if (!found) return false;
    for (const auto& row : ActivityVitalityRewardTableManager::Instance().FindAll().data())
        if (row.rule_id() == rule.id()) rewards.push_back(row);
    return true;
}

const PlayerActivityLedger* Ledger(entt::entity player) {
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    const auto* missions = container == nullptr ? nullptr : container->Get(MissionListComp::kPlayerMission);
    return missions == nullptr ? nullptr : &missions->GetMissionList().activity_ledger();
}

PlayerActivityLedger* MutableLedger(entt::entity player) {
    auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    auto* missions = container == nullptr ? nullptr : container->GetMutable(MissionListComp::kPlayerMission);
    return missions == nullptr ? nullptr : missions->GetMutableMissionList().mutable_activity_ledger();
}

uint32_t CheckIdentity(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    if (guid == nullptr || guid->value == 0 || guid->value == kInvalidGuid) return kInvalidParameter;
    if (Ledger(player) == nullptr) return kServiceUnavailable;
    return kSuccess;
}

bool ValidLedger(const PlayerActivityLedger& ledger) {
    if (ledger.sources_size() > kMaxLedgerRecords || ledger.claims_size() > kMaxLedgerRecords) return false;
    std::set<std::pair<uint32_t, uint32_t>> sources;
    std::set<std::tuple<uint32_t, uint64_t, uint32_t>> claims;
    std::set<std::string> requests;
    for (const auto& source : ledger.sources()) {
        if (source.mission_scope() != MissionListComp::kPlayerMission || source.mission_id() == 0 || source.rule_id() == 0 || source.configuration_revision() == 0 ||
            source.period_id() == 0 || source.points() == 0 ||
            !sources.emplace(source.mission_scope(), source.mission_id()).second) return false;
    }
    for (const auto& claim : ledger.claims()) {
        if (claim.rule_id() == 0 || claim.configuration_revision() == 0 || claim.period_id() == 0 ||
            claim.threshold_id() == 0 || claim.reward_id() == 0 || claim.claimed_at_ms() == 0 || claim.request_id().empty() ||
            claim.request_id().size() > 64 || !requests.insert(claim.request_id()).second ||
            !claims.emplace(claim.rule_id(), claim.period_id(), claim.threshold_id()).second)
            return false;
    }
    return true;
}

uint32_t RewardItems(uint32_t rewardId, ItemCountMap& items,
                     google::protobuf::RepeatedPtrField<PlayerActivityRewardItem>* display = nullptr) {
    items.clear();
    if (display != nullptr) display->Clear();
    const auto [reward, error] = RewardTableManager::Instance().FindByIdSilent(rewardId);
    if (rewardId == 0 || reward == nullptr) return kInvalidTableData;
    for (const auto& entry : reward->reward()) {
        const auto [item, itemError] = ItemTableManager::Instance().FindByIdSilent(entry.reward_item());
        auto& count = items[entry.reward_item()];
        if (item == nullptr || entry.reward_count() == 0 ||
            entry.reward_count() > std::numeric_limits<uint32_t>::max() - count) return kInvalidTableData;
        count += entry.reward_count();
    }
    if (items.empty()) return kInvalidTableData;
    if (display != nullptr) {
        std::vector<uint32_t> ids;
        for (const auto& [id, count] : items) ids.push_back(id);
        std::sort(ids.begin(), ids.end());
        for (const auto id : ids) {
            const auto [item, ignored] = ItemTableManager::Instance().FindByIdSilent(id);
            auto* shown = display->Add();
            shown->set_item_config_id(id);
            shown->set_count(items.at(id));
            shown->set_name(item->name());
            shown->set_icon_key(item->icon_key());
        }
    }
    return kSuccess;
}

bool ValidPolicy(const ActivityVitalityRuleTable& rule,
                 const std::vector<ActivityVitalityRewardTable>& rewards, uint64_t nowMs,
                 uint64_t& start, uint64_t& end) {
    if (!PlayerActivitySystem::ResolvePeriod(rule, nowMs, start, end) ||
        rewards.empty() || rewards.size() > kMaxThresholds) return false;
    std::set<uint32_t> ids;
    for (const auto& threshold : rewards) {
        ItemCountMap items;
        if (threshold.id() == 0 || threshold.rule_id() != rule.id() || threshold.required_points() == 0 ||
            threshold.required_points() > rule.points_limit() || !ids.insert(threshold.id()).second ||
            RewardItems(threshold.reward_id(), items) != kSuccess) return false;
    }
    return true;
}

bool SameIdentity(const PlayerActivityClaimRecord& record, const ClaimActivityVitalityRewardRequest& request) {
    return record.rule_id() == request.rule_id() && record.configuration_revision() == request.configuration_revision() &&
        record.period_id() == request.period_id() && record.threshold_id() == request.threshold_id();
}

const PlayerActivityClaimRecord* FindClaim(const PlayerActivityLedger& ledger, uint32_t ruleId,
                                         uint64_t period, uint32_t thresholdId) {
    for (const auto& claim : ledger.claims())
        if (claim.rule_id() == ruleId &&
            claim.period_id() == period && claim.threshold_id() == thresholdId) return &claim;
    return nullptr;
}

// 保留策略(docs/design/player-features-ui.md「领取身份、提交与重启恢复」):同一规则的周期锚点与长度不可改,
// 当前周期起点只会前进。别的规则算不出周期是否结束,它们的记录不动。
//
// 积分只按当前周期算,已结束周期的来源没有任何用处(一次性任务的去重靠 credited_missions 位图),全删。
void PruneEndedSources(PlayerActivityLedger& ledger, uint32_t ruleId, uint64_t period) {
    auto* sources = ledger.mutable_sources();
    sources->erase(std::remove_if(sources->begin(), sources->end(), [&](const PlayerActivityPointSource& source) {
        return source.rule_id() == ruleId && source.period_id() < period;
    }), sources->end());
}

// 已结束周期的领取记录不会再挡住任何新领取,只供重放原请求,留最近 kKeptPastClaims 条。
void PrunePastClaims(PlayerActivityLedger& ledger, uint32_t ruleId, uint64_t period) {
    const auto past = [&](const PlayerActivityClaimRecord& claim) { return claim.rule_id() == ruleId && claim.period_id() < period; };
    auto* claims = ledger.mutable_claims();
    auto excess = std::count_if(claims->begin(), claims->end(), past) - kKeptPastClaims;
    // 记录按领取先后追加,最老的在前。
    for (int i = 0; excess > 0;) {
        if (past(claims->Get(i))) { claims->DeleteSubrange(i, 1); --excess; }
        else ++i;
    }
}

bool Durable(entt::entity player, const PlayerActivityClaimRecord& claim) {
    const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    if (!player_mutability::HasFencedOwnership(player) || persisted == nullptr || !persisted->HasSnapshot() || guid == nullptr ||
        persisted->snapshot->player_database_data().player_id() != guid->value) return false;
    for (const auto& scope : persisted->snapshot->player_database_data().mission_component().scopes()) {
        if (scope.scope() != MissionListComp::kPlayerMission) continue;
        for (const auto& saved : scope.mission_list().activity_ledger().claims())
            if (saved.SerializeAsString() == claim.SerializeAsString()) return true;
    }
    return false;
}

uint32_t Points(const PlayerActivityLedger& ledger, const ActivityVitalityRuleTable& rule, uint64_t period) {
    uint64_t sum = 0;
    for (const auto& source : ledger.sources())
        if (source.rule_id() == rule.id() &&
            source.period_id() == period) sum = std::min<uint64_t>(rule.points_limit(), sum + source.points());
    return static_cast<uint32_t>(sum);
}

void Persist(entt::entity player, uint64_t nowMs, bool force) {
    if (!player_mutability::IsPlayerMutable(player)) return;
    // 此写入口延迟创建瞬时重存节流状态；持久账本仍只来自玩家加载边界。
    auto& state = tlsEcs.actorRegistry.get_or_emplace<ActivityPersistRequestComp>(player);
    if (!force && nowMs >= state.lastRequestMs && nowMs - state.lastRequestMs < kResaveIntervalMs) return;
    state.lastRequestMs = nowMs;
    (void)persistFn(player);
}
} // namespace

uint32_t PlayerActivitySystem::FillRewardDisplay(uint32_t rewardId, PlayerActivityInfo& out) {
    out.clear_reward_types();
    ItemCountMap items;
    if (rewardId == 0) { out.clear_rewards(); return kSuccess; }
    const auto result = RewardItems(rewardId, items, out.mutable_rewards());
    if (result != kSuccess) { out.clear_rewards(); return result; }
    std::set<PlayerActivityRewardType> types;
    for (const auto& [id, count] : items) {
        const auto [item, error] = ItemTableManager::Instance().FindByIdSilent(id);
        types.insert(item->equip_kind() > 0 ? PLAYER_ACTIVITY_REWARD_EQUIPMENT : PLAYER_ACTIVITY_REWARD_ITEM);
    }
    for (const auto type : types) out.add_reward_types(type);
    return kSuccess;
}

bool PlayerActivitySystem::ResolvePeriod(const ActivityVitalityRuleTable& rule, uint64_t nowMs,
                                        uint64_t& startsAtMs, uint64_t& endsAtMs) {
    startsAtMs = endsAtMs = 0;
    const uint64_t first = rule.first_period_starts_at_ms();
    const uint64_t duration = rule.period_duration_ms();
    if (!rule.enabled() || rule.id() == 0 || rule.configuration_revision() == 0 || rule.points_limit() == 0 ||
        first == 0 || duration == 0 || nowMs < first) return false;
    const uint64_t start = nowMs - (nowMs - first) % duration;
    if (duration > std::numeric_limits<uint64_t>::max() - start) return false;
    startsAtMs = start;
    endsAtMs = start + duration;
    return true;
}

void PlayerActivitySystem::OnMissionCompleted(entt::entity player, uint32_t scope, uint32_t missionId, uint64_t nowMs) {
    // 与任务完成位同帧记录来源；不能额外套资产可变性闸而永久漏掉一次性完成通知。
    // 无围栏/退出状态仍不能领取资产，领取权只在 Claim 的强归属提交点裁决。
    if (CheckIdentity(player) != kSuccess) return;
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    const auto* missions = container->Get(scope);
    if (scope != MissionListComp::kPlayerMission || missions == nullptr || !missions->IsComplete(missionId)) return;
    const auto [mission, missionError] = MissionTableManager::Instance().FindByIdSilent(missionId);
    if (mission == nullptr || mission->mission_type() != 2) return;
    const auto [activity, error] = ActivityScheduleTableManager::Instance().FindByIdSilent(missionId);
    if (activity == nullptr || activity->vitality_points() == 0 || activity->definition_key().empty() ||
        activity->configuration_revision() == 0) return;
    ActivityVitalityRuleTable rule;
    std::vector<ActivityVitalityRewardTable> rewards;
    uint64_t start, end;
    // 奖励表暂时异常只禁领，不改变已经启用的积分规则和事实发生的周期。
    if (!LoadPolicy(rule, rewards) || !ResolvePeriod(rule, nowMs, start, end)) return;
    auto* ledger = MutableLedger(player);
    // 任务表 id 列带位序标记,每个任务都有位序;没有就是导表产物不一致,不记(记了也没法去重)。
    const auto bit = GetBitIndex(MissionBitMap, missionId);
    if (!ValidLedger(*ledger) || !bit.has_value()) {
        LOG_ERROR << "Activity vitality source not recorded: ledger invalid or mission has no bit index, mission=" << missionId;
        return;
    }
    // 一次性任务只记一次,跨周期也一样(docs/design/player-features-ui.md「正式活跃配置与来源」)。
    if (TestPackedBit(ledger->credited_missions(), *bit)) return;
    PruneEndedSources(*ledger, rule.id(), start);
    if (ledger->sources_size() >= kMaxLedgerRecords) {
        LOG_ERROR << "Activity vitality source not recorded: current period already has " << ledger->sources_size()
                  << " sources, mission=" << missionId;
        return;
    }
    // 去重位与来源同帧写入、同一次存盘提交。
    SetPackedBit(*ledger->mutable_credited_missions(), *bit);
    auto* source = ledger->add_sources();
    source->set_mission_scope(scope);
    source->set_mission_id(missionId);
    source->set_rule_id(rule.id());
    source->set_configuration_revision(rule.configuration_revision());
    source->set_period_id(start);
    source->set_points(activity->vitality_points());
}

uint32_t PlayerActivitySystem::BuildVitality(entt::entity player, uint64_t nowMs, PlayerActivityVitalitySnapshot& out) {
    out.Clear();
    if (const auto error = CheckIdentity(player); error != kSuccess) return error;
    const auto* ledger = Ledger(player);
    if (!ValidLedger(*ledger)) return kInvalidTableData;
    ActivityVitalityRuleTable rule;
    std::vector<ActivityVitalityRewardTable> rewards;
    uint64_t start, end;
    if (!LoadPolicy(rule, rewards)) {
        out.set_unavailable_reason("活跃规则与阈值奖励尚未配置");
        return kSuccess;
    }
    if (!ValidPolicy(rule, rewards, nowMs, start, end)) {
        out.set_unavailable_reason("活跃配置异常或尚未开放");
        return kSuccess;
    }
    out.set_available(true);
    out.set_rule_id(rule.id());
    out.set_configuration_revision(rule.configuration_revision());
    out.set_period_id(start);
    out.set_period_starts_at_ms(start);
    out.set_period_ends_at_ms(end);
    out.set_points(Points(*ledger, rule, start));
    out.set_points_limit(rule.points_limit());
    std::sort(rewards.begin(), rewards.end(), [](const auto& left, const auto& right) {
        return std::pair{left.required_points(), left.id()} < std::pair{right.required_points(), right.id()};
    });
    for (const auto& threshold : rewards) {
        auto* shown = out.add_rewards();
        shown->set_threshold_id(threshold.id());
        shown->set_required_points(threshold.required_points());
        shown->set_reward_id(threshold.reward_id());
        ItemCountMap items;
        (void)RewardItems(threshold.reward_id(), items, shown->mutable_items());
        const auto* claim = FindClaim(*ledger, rule.id(), start, threshold.id());
        shown->set_claimed(claim != nullptr);
        shown->set_durable(claim != nullptr && Durable(player, *claim));
        shown->set_can_claim(claim == nullptr && out.points() >= threshold.required_points() &&
            ledger->claims_size() < kMaxLedgerRecords && player_mutability::IsPlayerMutable(player) &&
            !tlsEcs.actorRegistry.any_of<InBattleComp>(player));
        if (claim != nullptr) shown->set_unavailable_reason(shown->durable() ? "已领取" : "奖励保存中，请重查原领取");
        else if (out.points() < threshold.required_points()) shown->set_unavailable_reason("活跃积分尚未达到门槛");
        else if (!shown->can_claim()) shown->set_unavailable_reason("当前状态暂不可领取");
    }
    return kSuccess;
}

uint32_t PlayerActivitySystem::Claim(entt::entity player, uint64_t nowMs,
                                    const ClaimActivityVitalityRewardRequest& request,
                                    ClaimActivityVitalityRewardResponse& response) {
    response.Clear();
    response.set_request_id(request.request_id());
    if (const auto error = CheckIdentity(player); error != kSuccess) return error;
    const auto* ledger = Ledger(player);
    if (!ValidLedger(*ledger)) return kInvalidTableData;
    if (request.request_id().empty() || request.request_id().size() > 64 || request.rule_id() == 0 ||
        request.configuration_revision() == 0 || request.period_id() == 0 || request.threshold_id() == 0) return kInvalidParameter;
    // 原请求优先于跨日、下架、冻结和规则热更新；冲突身份绝不另发。
    for (const auto& existing : ledger->claims()) {
        if (existing.request_id() != request.request_id()) continue;
        if (!SameIdentity(existing, request)) return kInvalidParameter;
        response.set_claimed(true);
        response.set_replayed(true);
        if (!Durable(player, existing)) Persist(player, nowMs, false);
        response.set_durable(Durable(player, existing));
        (void)BuildVitality(player, nowMs, *response.mutable_snapshot());
        return kSuccess;
    }
    if (FindClaim(*ledger, request.rule_id(), request.period_id(), request.threshold_id()) != nullptr)
        return kRewardAlreadyClaimed;
    if (!player_mutability::IsPlayerMutable(player) || tlsEcs.actorRegistry.any_of<InBattleComp>(player)) return kFeatureUnavailable;
    ActivityVitalityRuleTable rule;
    std::vector<ActivityVitalityRewardTable> rewards;
    uint64_t start, end;
    if (!LoadPolicy(rule, rewards)) return kFeatureUnavailable;
    if (!ValidPolicy(rule, rewards, nowMs, start, end)) return kInvalidTableData;
    // 周期已结束单列一个码:跨日边界上发出的请求会落到这里;原请求的记录被裁掉之后的重放也落到这里,
    // 这时无法判断当初领没领到,码的文案让玩家以背包为准。
    if (request.rule_id() == rule.id() && request.period_id() < start) return kRewardPeriodEnded;
    if (request.rule_id() != rule.id() || request.configuration_revision() != rule.configuration_revision() || request.period_id() != start)
        return kInvalidParameter;
    const auto threshold = std::find_if(rewards.begin(), rewards.end(), [&](const auto& row) { return row.id() == request.threshold_id(); });
    if (threshold == rewards.end() || Points(*ledger, rule, start) < threshold->required_points()) return kInvalidParameter;
    // 先裁再判预算:否则每周期每档一条,攒满之后这个角色再也领不了。裁掉的记录不会再用到,后面发放失败也不用还原。
    PrunePastClaims(*MutableLedger(player), rule.id(), start);
    if (ledger->claims_size() >= kMaxLedgerRecords) return kServiceUnavailable;
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    ItemCountMap items;
    if (const auto error = RewardItems(threshold->reward_id(), items); error != kSuccess) return error;
    // BagService 先预检整批容量/禁获/号段；失败不改背包或消耗领取权。此后无异步间隙。
    if (const auto error = BagService::AddItems(player, bags->bags[kInventory], items, TX_QUEST_REWARD); error != kSuccess) return error;
    auto* claim = MutableLedger(player)->add_claims();
    claim->set_rule_id(rule.id());
    claim->set_configuration_revision(rule.configuration_revision());
    claim->set_period_id(start);
    claim->set_threshold_id(threshold->id());
    claim->set_request_id(request.request_id());
    claim->set_reward_id(threshold->reward_id());
    claim->set_claimed_at_ms(nowMs);
    response.set_claimed(true);
    Persist(player, nowMs, true);
    response.set_durable(Durable(player, *claim));
    (void)BuildVitality(player, nowMs, *response.mutable_snapshot());
    return kSuccess;
}

void PlayerActivitySystem::SetRuleLookupForTest(RuleLookupFn fn) { ruleLookup = fn; }
void PlayerActivitySystem::SetPersistForTest(PersistFn fn) { persistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn; }
