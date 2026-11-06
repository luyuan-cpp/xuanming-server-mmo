#include "player_feature_snapshot.h"

#include <algorithm>
#include <limits>
#include <set>
#include <utility>

#include "muduo/base/Logging.h"
#include "thread_context/ecs_context.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/condition/condition_util.h"
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:回合制战斗在途拒绝整理背包
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "table/code/condition_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"     // kBagTemporary* / kBagAddItemBagFull:临时格取回
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "services/scene/player/system/player_equip.h"
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen:临时格取回的交接冻结闸
#include "services/scene/player/system/player_inbox_system.h" // OnBagSpaceFreed:取回之后提前重试延后的结算
#include "services/scene/player/system/player_mission.h"
#include "services/scene/player/system/player_activity_schedule.h"
#include "services/scene/player/system/player_activity.h"
#include "engine/core/time/system/time.h"

namespace {

bool CanSortBagType(uint32_t bagType)
{
    return bagType == kInventory || bagType == kWarehouse;
}

void FillMission(entt::entity player, uint64_t nowMs, uint32_t missionId, uint32_t scope, const MissionsComp* missions,
                 const MissionTable* row, PlayerMissionInfo& out)
{
    out.set_mission_id(missionId);
    out.set_scope(scope);
    out.set_configured(row != nullptr);
    const MissionComp* active = nullptr;
    if (missions != nullptr)
    {
        const auto it = missions->GetMissionList().missions().find(missionId);
        if (it != missions->GetMissionList().missions().end()) active = &it->second;
        if (missions->IsClaimable(missionId)) out.set_status(PLAYER_MISSION_CLAIMABLE);
        else if (missions->IsComplete(missionId)) out.set_status(PLAYER_MISSION_COMPLETED);
        else if (active != nullptr)
        {
            const bool failed = active->status() == MissionComp::E_MISSION_TIME_OUT ||
                                active->status() == MissionComp::E_MISSION_FAILD;
            out.set_status(failed ? PLAYER_MISSION_FAILED : PLAYER_MISSION_ACTIVE);
        }
    }

    out.set_can_accept(false);
    out.set_can_claim(false);
    if (row == nullptr) {
        out.set_unavailable_reason("任务配置暂不可用");
        return;
    }
    const auto accept = PlayerMissionSystem::CheckAccept(player, scope, missionId, nowMs);
    const auto claim = PlayerMissionSystem::CheckClaim(player, scope, missionId);
    out.set_can_accept(out.status() == PLAYER_MISSION_NOT_ACCEPTED && accept == kSuccess);
    out.set_can_claim(out.status() == PLAYER_MISSION_CLAIMABLE && claim == kSuccess);
    if (!out.can_accept() && !out.can_claim()) {
        if (out.status() == PLAYER_MISSION_ACTIVE) out.set_unavailable_reason("完成任务目标后领取奖励");
        else if (out.status() == PLAYER_MISSION_COMPLETED) out.set_unavailable_reason("任务已完成");
        else if (out.status() == PLAYER_MISSION_FAILED) out.set_unavailable_reason("任务已失效");
        else if (out.status() == PLAYER_MISSION_CLAIMABLE) out.set_unavailable_reason("当前状态暂不可领奖，请稍后重试");
        else if (accept == kMissionTypeAlreadyExists) out.set_unavailable_reason("请先完成同类型任务");
        else if (row->mission_type() == 2) {
            PlayerActivityInfo schedule;
            PlayerActivityScheduleSystem::BuildInfo(missionId, nowMs, schedule);
            out.set_unavailable_reason(schedule.unavailable_reason());
        }
        else if (accept == kServiceUnavailable) out.set_unavailable_reason("任务所需玩法暂未开放");
        else out.set_unavailable_reason("当前条件下暂不可接取");
    }
    out.set_mission_type(row->mission_type());
    out.set_mission_sub_type(row->mission_sub_type());
    out.set_reward_id(row->reward_id());
    out.set_auto_reward(row->auto_reward() != 0);

    // 进度槽与原始目标索引一致，缺失条件不能挤占后续目标的进度。
    for (int index = 0; index < row->condition_id_size(); ++index)
    {
        const uint32_t conditionId = row->condition_id(index);
        auto* objective = out.add_objectives();
        objective->set_objective_index(static_cast<uint32_t>(index));
        objective->set_condition_id(conditionId);
        const auto [condition, lookupResult] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
        if (condition == nullptr) continue;
        const uint32_t overrideTarget = index < row->target_count_size() ? row->target_count(index) : 0;
        const uint32_t target = overrideTarget > 0 ? overrideTarget : condition->target_count();
        const uint32_t progress = active != nullptr && index < active->progress_size()
            ? active->progress(index) : 0;
        objective->set_category(condition->condition_category());
        objective->set_target(target);
        objective->set_progress(progress);
        // 已完成记录没有历史计数；不伪造计数，完成位单独表达。
        objective->set_completed(out.status() == PLAYER_MISSION_COMPLETED ||
            out.status() == PLAYER_MISSION_CLAIMABLE ||
            (active != nullptr && condition_util::IsFulfilled(conditionId, progress, overrideTarget)));
    }
}

} // namespace

uint32_t PlayerBagSystem::BuildSnapshot(entt::entity player, uint32_t bagType, BagInfo& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (bagType >= kBagTypeCount) return kInvalidParameter;
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    const auto& bag = bags->bags[bagType];
    if (!bag.IsLayerConsistent() || bag.Capacity() > std::numeric_limits<uint32_t>::max())
        return kInvalidTableData;

    auto* layout = out.mutable_layout();
    layout->set_bag_type(bagType);
    layout->set_capacity(static_cast<uint32_t>(bag.Capacity()));
    layout->set_can_sort(CanSortBagType(bagType));
    bag.ForEachItem([&out, layout, &bag](Guid guid, const ItemComp& item) {
        auto* info = out.add_items();
        info->set_item_id(guid);
        info->set_config_id(item.config_id());
        info->set_count(item.size());
        const auto [row, lookupResult] = ItemTableManager::Instance().FindByIdSilent(item.config_id());
        if (row != nullptr)
        {
            info->set_max_stack(row->max_stack_size());
            info->set_equip_kind(row->equip_kind());
        }
        // 名称 / 描述 / 图标 / 佩戴要求 / 属性行(BagItemInfo 6–13 号字段)。客户端不加载配表,
        // 装备 tooltip 上的每一个字都来自这里;上限按当前表现查,所以改表后下一次开包就生效。
        PlayerEquipSystem::FillItemDisplay(item, *info);
        auto* slot = layout->add_slots();
        slot->set_slot(bag.GetItemPosByGuid(guid));
        slot->set_item_id(guid);
        const auto footprint = bag.GetItemFootprintByGuid(guid);
        slot->set_width(footprint.width);
        slot->set_height(footprint.height);
    });
    std::sort(out.mutable_items()->begin(), out.mutable_items()->end(),
        [](const BagItemInfo& left, const BagItemInfo& right) { return left.item_id() < right.item_id(); });
    std::sort(layout->mutable_slots()->begin(), layout->mutable_slots()->end(),
        [](const BagSlotInfo& left, const BagSlotInfo& right) { return left.slot() < right.slot(); });
    // 装备栏额外下发槽位定义(含空槽):客户端据此画出栏位、在空槽上显示部位名。
    // 必须在 set_capacity 与上面填完 slots 之后调 —— 它只下发槽号在容量内的槽,
    // 并且没有部位名的槽只在被占用时才下发(占用关系读 layout.slots)。
    if (bagType == kEquipment) PlayerEquipSystem::FillEquipSlots(*layout);
    if (const auto* currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player))
        out.mutable_currency()->CopyFrom(*currency);
    return kSuccess;
}

uint32_t PlayerBagSystem::Sort(entt::entity player, uint32_t bagType, BagInfo& out, bool& changed)
{
    out.Clear();
    changed = false;
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (!CanSortBagType(bagType)) return kInvalidParameter;
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    auto& bag = bags->bags[bagType];
    if (!bag.IsLayerConsistent()) return kInvalidTableData;
    // 回合制战斗在途拒绝整理(moba-battle-target-architecture.md 红线 2)。
    // 整理会合并堆并退役一批 item_uuid,而结算扣除的流水正是按 item_uuid 关联的;
    // 战斗中整理会让"快照那一刻的实例集合"与结算落地时的对不上。
    if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
    {
        return kInvalidParameter;
    }

    // 跨节点交接冻结闸不在这里:它在 BagService::MergeAndCompact 开头(所有整理入口共用,先于任何改动),
    // 冻结中同样回 kInvalidParameter;回归用例 PlayerFeatureSnapshotTest.FrozenSortKeepsSourceLayoutUnchanged。
    const uint32_t result = BagService::SortByPlayerRequest(player, bag, &changed);
    if (result != kSuccess) return result;
    return BuildSnapshot(player, bagType, out);
}

uint32_t PlayerBagSystem::RetrieveTemporary(entt::entity player, uint64_t itemId, bool takeAll)
{
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    // 纯参数校验先于状态校验:两样都给或都不给永远不可能成功,不该被报成「稍后再试」。
    if ((itemId != 0) == takeAll) return kInvalidParameter;
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    const auto& temporary = bags->bags[kTemporary];
    if (!temporary.IsLayerConsistent() || !bags->bags[kInventory].IsLayerConsistent()) return kInvalidTableData;
    // 只拦交接冻结:冻结中改了也不会存盘(BagService 那一道只是兜底)。
    // **战斗中放行**(2026-11-06 评审修正):两包都放不下而整笔延后的那一局,InBattleComp 要等结算应用并确认提交之后
    // 才摘(CompleteInboxSettlement)。战斗中也拒的话,玩家在冻结到期之前取不回、腾不出位置,延后的结算就一直补不上。
    // 放行是安全的:人物背包只进不出(冻结清单「只增不减,方向安全」),战斗快照与结算扣消耗都只看人物背包,
    // 结算按持有夹紧时持有只会更多、不会少扣;临时格不参战;人物背包里已有的实例一个都不退役(整理在局中拒,正因为它会)。
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
        return kBagTemporaryRetrieveBusy;

    std::vector<Guid> guids;
    if (takeAll)
    {
        // 按入包先后(ItemStore::ForEach 的实例序):全或无,顺序只决定落在哪几格、先灌哪一堆,结果确定。
        temporary.ForEachItem([&guids](Guid guid, const ItemComp&) { guids.push_back(guid); });
        if (guids.empty()) return kSuccess; // 重发的「全部取回」在第一次已经成功时落到这里:什么都不动
    }
    else
    {
        // 「上一次其实已经取回、回包丢了」的重发也落在这里:东西已经在人物背包,两个包都不动。
        if (!temporary.ContainsGuid(itemId)) return kBagTemporaryItemNotFound;
        guids.push_back(itemId);
    }
    const uint32_t result = BagService::RetrieveFromTemporary(player, guids);
    if (result == kSuccess)
    {
        // 临时格腾出了位置:因两包都放不下而延后的收件箱条目(战斗结算)下一个 Tick 就重试,不用等退避(§6.6)。
        // 只是把下一次尝试提前,不改任何判定;单件取回与全部取回都走到这里(全部取回时临时格本来就空的,上面已提前返回)。
        PlayerInboxSystem::OnBagSpaceFreed(player);
        return kSuccess;
    }
    // 人物背包放不下是玩家随手能触发的常规拒绝:换成这个入口自己的码,客户端提示腾位或改成一件一件取。
    return result == kBagAddItemBagFull ? uint32_t{kBagTemporaryRetrieveBagFull} : result;
}

uint32_t PlayerMissionReadSystem::BuildList(entt::entity player, GetMissionListResponse& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    if (container == nullptr)
    {
        LOG_ERROR << "[PlayerMission] 缺少持久 MissionsContainerComp，拒绝任务列表读取，entity="
                  << entt::to_integral(player);
        return kServiceUnavailable;
    }
    std::set<std::pair<uint32_t, uint32_t>> keys;
    for (const auto& row : MissionTableManager::Instance().FindAll().data())
        keys.emplace(MissionListComp::kPlayerMission, row.id());
    for (const auto& [scope, missions] : container->map)
    {
        for (const auto& [id, mission] : missions.GetMissionList().missions()) keys.emplace(scope, id);
        for (const auto& [id, bitIndex] : MissionBitMap)
            if (missions.IsComplete(id) || missions.IsClaimable(id)) keys.emplace(scope, id);
        for (const auto id : missions.GetUnmappedCompletedIds()) keys.emplace(scope, id);
        for (const auto id : missions.GetUnmappedClaimableIds()) keys.emplace(scope, id);
    }
    for (const auto& [scope, id] : keys)
    {
        const auto [row, lookupResult] = MissionTableManager::Instance().FindByIdSilent(id);
        FillMission(player, TimeSystem::NowMillisecondsUTC(), id, scope, container->Get(scope), row, *out.add_missions());
    }
    out.set_state_persistent(true);
    return kSuccess;
}

uint32_t PlayerActivityReadSystem::BuildList(entt::entity player, uint64_t serverTimeMs,
                                           GetActivityListResponse& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (!tlsEcs.actorRegistry.all_of<MissionsContainerComp>(player))
    {
        LOG_ERROR << "[PlayerActivity] 缺少持久 MissionsContainerComp，拒绝活动列表读取，entity="
                  << entt::to_integral(player);
        return kServiceUnavailable;
    }
    out.set_server_time_ms(serverTimeMs);
    if (const auto result = PlayerActivitySystem::BuildVitality(player, serverTimeMs, *out.mutable_vitality()); result != kSuccess)
        return result;
    const auto* missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player).Get(MissionListComp::kPlayerMission);
    for (const auto& row : MissionTableManager::Instance().FindAll().data())
    {
        if (row.mission_type() != 2) continue;
        auto* info = out.add_activities();
        const auto scheduleResult = PlayerActivityScheduleSystem::BuildInfo(row.id(), serverTimeMs, *info);
        if (scheduleResult != kSuccess) {
            // 保留目录身份以解释异常配置，不能把清空后的 id=0 伪装成新活动。
            info->set_activity_id(row.id());
            info->set_mission_id(row.id());
            continue;
        }
        // 现有任务不可重复接取；这里的次数是每角色终身一次，绝不是每日目标计数。
        info->set_completion_count_known(missions != nullptr);
        info->set_completed_count(missions != nullptr && missions->IsComplete(row.id()) ? 1 : 0);
        info->set_completion_limit(1);
        info->set_completion_period("每角色一次");
        if (const auto rewardResult = PlayerActivitySystem::FillRewardDisplay(row.reward_id(), *info); rewardResult != kSuccess) {
            info->set_unavailable_reason("活动奖励配置异常，暂不可参与");
            info->set_can_participate(false);
            continue;
        }
        const auto acceptance = PlayerMissionSystem::CheckAccept(
            player, MissionListComp::kPlayerMission, row.id(), serverTimeMs);
        info->set_can_participate(info->status() == PLAYER_ACTIVITY_OPEN && acceptance == kSuccess);
        if (info->status() == PLAYER_ACTIVITY_OPEN && !info->can_participate()) {
            if (acceptance == kMissionIdRepeated || acceptance == kMissionAlreadyCompleted)
                info->set_unavailable_reason("请在任务页查看活动进度");
            else if (acceptance == kMissionTypeAlreadyExists)
                info->set_unavailable_reason("请先完成同类型活动任务");
            else info->set_unavailable_reason("活动所需玩法或当前状态暂不满足参与条件");
        }
    }
    std::sort(out.mutable_activities()->begin(), out.mutable_activities()->end(),
        [](const PlayerActivityInfo& left, const PlayerActivityInfo& right) {
            return left.activity_id() < right.activity_id();
        });
    return kSuccess;
}
