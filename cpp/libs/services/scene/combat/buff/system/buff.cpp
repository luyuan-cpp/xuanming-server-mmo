#include "buff.h"
#include <ranges>
#include "table/code/buff_table.h"
#include "combat/buff/system/buff_impl.h"
#include "proto/common/component/buff_comp.pb.h"
#include <muduo/base/Logging.h>
#include "table/proto/tip/buff_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "macros/error_return.h"
#include "modifier_buff_impl.h"
#include "motion_modifier_impl.h"
#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include "proto/common/event/skill_event.pb.h"
#include "core/utils/utility/utility.h"
#include "player/comp/player_frozen_comp.h" // Frozen exclude — cross-zone-readiness-audit.md §11.2
#include "core/system/id_generator.h"
#include <thread_context/ecs_context.h>


// TODO: Combat logic must run on frames, not timers. This ensures buff triggers and expirations
// happen within frame logic, avoiding issues where timer callbacks fire after entity
// destruction or buff removal. Consider migrating all buff expiry and periodic triggers
// to per-frame processing.

uint64_t GenerateUniqueBuffId(const BuffListComp& buffList)
{
    uint64_t newBuffId = UINT64_MAX;
    do {
        newBuffId = tlsIdGeneratorManager.buffIdGenerator.Generate();
    } while (buffList.contains(newBuffId) || newBuffId == UINT64_MAX);
    return newBuffId;
}

bool IsTargetImmune(const BuffListComp& buffList, const BuffTable* buffTableParam)
{
    for (const auto& buff : buffList | std::views::values) {
        // 这里原来用的是 LookupBuffOrReturnError —— 它 `return buffResult`(uint32
        // 错误码),在这个返回 bool 的函数里会被隐式转成 true。于是身上任何一条
        // buff 掉了表行,目标就被判成"对一切 buff 免疫",后续所有加 buff 静默失败。
        // 查不到行只应该跳过这一条。
        LookupBuffOrContinue(buff.buffPb.buff_table_id());
        for (const auto& tag : buffTableParam->tag() | std::views::keys) {
            if (buffRow->immune_tag().contains(tag)) {
                return true;
            }
        }
    }
    return false;
}

BuffMessagePtr CreateBuffDataPtr(const BuffTable* buffTable) {
    switch (buffTable->buff_type()) {
    case kBuffTypeNoDamageOrSkillHitInLastSeconds:
        return std::make_shared<BuffNoDamageOrSkillHitInLastSecondsComp>();
    default:
        return nullptr;
    }
}

// True if the entity still has a stealth buff other than the given one.
bool HasStealthBuffExcept(const entt::entity parent, const uint64_t excludeBuffId)
{
    const auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
    if (!buffList) {
        return false;
    }

    for (const auto& entry : *buffList | std::views::values) {
        if (entry.buffPb.buff_id() == excludeBuffId) {
            continue;
        }
        const auto [tbl, res] = BuffTableManager::Instance().FindById(entry.buffPb.buff_table_id());
        if (tbl && tbl->buff_type() == kBuffTypeStealth) {
            return true;
        }
    }
    return false;
}

// Add or update buff (with abilityContext)
std::tuple<uint32_t, uint64_t> BuffSystem::AddOrUpdateBuff(
    const entt::entity parent,
    const uint32_t buffTableId,
    const SkillContextPtrComp& abilityContext)
{
    if (!tlsEcs.actorRegistry.valid(parent))
    {
        return {kThisEntityIsInvalid, UINT64_MAX};
    }

    // 冻结中的目标(跨 zone 迁移在途):buff 列表已经在 HandleCrossZoneTransfer
    // 打进 PlayerAllData,目的 zone 会照快照重建。源端此时再加 buff,只会造出一条
    // 只有这个将死的源端实体看得见的孤儿。cross-zone-readiness-audit.md §11.4 的
    // 默认策略是 "buff drop"。
    //
    // 这个判定原来写在 OnBuffStart 里 —— 但那时 buff 已经 emplace 进 buffList 了,
    // 所谓"丢弃"实际是:条目在、OnBuffStart 的效果一个没跑、到期定时器照挂,而且
    // 它还会被 marshal 到目的 zone 复活成一条从没启动过的 buff。必须在插入前拒掉。
    if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(parent))
    {
        LOG_INFO << "[CrossZone] Buff on frozen target dropped. parent="
                 << entt::to_integral(parent) << " buff_table=" << buffTableId;
        return {kSuccess, UINT64_MAX};
    }

    LookupBuffOrReturn(buffTableId, (std::make_tuple(buffResult, UINT64_MAX)));

    if (const auto result = CanCreateBuff(parent, buffTableId); result != kSuccess) {
        return {result, UINT64_MAX};
    }

    auto& buffList = tlsEcs.actorRegistry.get_or_emplace<BuffListComp>(parent);

    // Pure dispel buffs and stack/refresh of an existing buff don't create a new entry.
    if (DispelBuffsOnAwake(parent, buffTableId)) {
        return {kSuccess, UINT64_MAX};
    }

    if (HandleExistingBuff(parent, buffTableId, abilityContext)) {
        return {kSuccess, UINT64_MAX};
    }

    BuffEntry newBuff;
    if (abilityContext != nullptr)
    {
        newBuff.buffPb.set_caster(abilityContext->caster());
    }
    newBuff.buffPb.set_processed_caster(buffRow->no_caster() ? entt::null : (abilityContext ? abilityContext->caster() : entt::null));

    uint64_t newBuffId = GenerateUniqueBuffId(buffList);
    newBuff.buffPb.set_buff_id(newBuffId);
    newBuff.buffPb.set_buff_table_id(buffTableId);
    newBuff.skillContext = abilityContext;
    newBuff.dataPbPtr = CreateBuffDataPtr(buffRow);

    auto [fst, snd] = buffList.emplace(newBuffId, std::move(newBuff));
    OnBuffStart(parent, fst->second, buffRow);

    if (buffRow->duration() > 0) {
        fst->second.expireTimerTaskComp.RunAfter(buffRow->duration(), [parent, newBuffId] {
            if (!tlsEcs.actorRegistry.valid(parent))
            {
                return;
            }
            OnBuffExpire(parent, newBuffId);
            });
    }
    else if (IsZero(buffRow->duration())) {
        OnBuffExpire(parent, newBuffId);
    }

    return {kSuccess, newBuffId};
}

// Add or update buff (without abilityContext)
std::tuple<uint32_t, uint64_t> BuffSystem::AddOrUpdateBuff(
    const entt::entity parent,
    const uint32_t buffTableId)
{
    return AddOrUpdateBuff(parent, buffTableId, nullptr);
}


// Remove buff
void BuffSystem::RemoveBuff(const entt::entity parent, const uint64_t buffId)
{
    OnBuffExpire(parent, buffId);
}

void BuffSystem::RemoveBuff(const entt::entity parent, const UInt64Set& removeBuffIdList) {
    for (auto& removeBuffId : removeBuffIdList) {
        BuffSystem::RemoveBuff(parent, removeBuffId);
    }
}

void BuffSystem::RemoveSubBuff(BuffEntry& buffComp, UInt64Set& buffsToRemove)
{
    for (auto& [subBuffId, _] : buffComp.buffPb.sub_buff_list_id())
    {
        buffsToRemove.emplace(subBuffId);
    }

    buffComp.buffPb.clear_sub_buff_list_id();
}

void BuffSystem::MarkBuffForRemoval(const entt::entity parent, uint64_t buffId) {
    auto& pendingRemoveBuffs = tlsEcs.actorRegistry.get_or_emplace<BuffPendingRemoveBuffs>(parent);
    pendingRemoveBuffs.emplace(buffId);
}

// Remove pending buffs at end of frame
void BuffSystem::RemovePendingBuffs(const entt::entity parent, BuffListComp& buffListComp) {
    // 这里原来是 get_or_emplace:BuffSystem::Update 每帧对每个带 buff 的实体都调一次,
    // 于是所有带 buff 的实体都会被永久挂上一个空的 unordered_set 组件(而且
    // MarkBuffForRemoval 全仓无调用方,这个集合永远是空的)。CLAUDE.md §7.5 明确
    // 禁止 per-tick 路径用 get_or_emplace,改 try_get + 早退。
    auto* pendingRemoveBuffs = tlsEcs.actorRegistry.try_get<BuffPendingRemoveBuffs>(parent);
    if (pendingRemoveBuffs == nullptr || pendingRemoveBuffs->empty()) {
        return;
    }

    for (const auto& buffId : *pendingRemoveBuffs) {
        buffListComp.erase(buffId);
        LOG_TRACE << "Buff with ID " << buffId << " removed from entity at end of frame.\n";
    }

    pendingRemoveBuffs->clear();
}

// Buff expiry handler
void BuffSystem::OnBuffExpire(const entt::entity parent, const uint64_t buffId)
{
    auto *buffListPtr = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
    if (!buffListPtr)
    {
        LOG_ERROR << "Cannot find buff list for entity " << entt::to_integral(parent);
        return;
    }
    auto &buffList = *buffListPtr;
    const auto buffIt = buffList.find(buffId);

    if (buffIt == buffList.end()) {
        LOG_ERROR << "Cannot find buff " << buffId;
        return;
    }

    const auto buffTableId = buffIt->second.buffPb.buff_table_id();
    LookupBuffOrReturnVoid(buffTableId);

    OnBuffRemove(parent, buffIt->second, buffRow);
    buffList.erase(buffId);
    OnBuffDestroy(parent, buffId, buffRow);
}

// Check if buff can be created
uint32_t BuffSystem::CanCreateBuff(const entt::entity parentEntity, const uint32_t buffTableId)
{
    LookupBuffOrReturnError(buffTableId);

    const auto *buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parentEntity);
    if (buffList)
    {
        if (const bool isImmune = IsTargetImmune(*buffList, buffRow))
        {
            return MAKE_ERROR_MSG(kBuffTargetImmuneToBuff,
                                  "entity=" << entt::to_integral(parentEntity)
                                            << " buffTableId=" << buffTableId);
        }
    }

    return kSuccess;
}

// Handle existing buff (stack/refresh)
bool BuffSystem::HandleExistingBuff(const entt::entity parentEntity,
    const uint32_t buffTableId,
    const SkillContextPtrComp& abilityContext)
{
    LookupBuffOrReturnFalse(buffTableId);

    if (!abilityContext) {
        return false;
    }

    auto& buffList = tlsEcs.actorRegistry.get_or_emplace<BuffListComp>(parentEntity);
    for (auto& buffComp : buffList | std::views::values) {
        if (buffComp.buffPb.buff_table_id() == buffTableId && buffComp.buffPb.processed_caster() == abilityContext->caster()) {
            if (buffComp.buffPb.layer() < buffRow->max_layer()) {
                buffComp.buffPb.set_layer(buffComp.buffPb.layer() + 1);
            }
            OnBuffRefresh(parentEntity, buffTableId, abilityContext, buffComp);
            return true;
        }
    }
    return false;
}

// Dispel matching buffs on awake; returns true if this is a pure Dispel buff
// (consumed without being added to the buff list).
bool BuffSystem::DispelBuffsOnAwake(const entt::entity parent, const uint32_t buffTableId)
{
    const auto [addBuffRow, addBuffResult] = BuffTableManager::Instance().FindByIdSilent(buffTableId);
    if (!addBuffRow) {
        LOG_ERROR << "Buff row not found for ID: " << buffTableId;
        return false;
    }

    UInt64Vector dispelBuffIdList;
    auto *buffListPtr = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
    if (buffListPtr == nullptr) {
        return addBuffRow->buff_type() == kBuffTypeDispel;
    }
    auto &buffList = *buffListPtr;
    for (auto& [buffId, entry] : buffList) {
        LookupBuffOrContinue(entry.buffPb.buff_table_id());
        for (const auto& dispelTag : addBuffRow->dispel_tag() | std::views::keys) {
            if (buffRow->tag().contains(dispelTag)) {
                dispelBuffIdList.emplace_back(buffId);
                break;
            }
        }
    }

    for (const auto& buffId : dispelBuffIdList) {
        BuffSystem::OnBuffExpire(parent, buffId);
    }

    return addBuffRow->buff_type() == kBuffTypeDispel;
}

void BuffSystem::OnBuffStart(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable)
{
    // 冻结目标的 "buff drop" 判定已上移到 AddOrUpdateBuff 的入口(插入之前),
    // 见那里的注释。

    // Maintain stealth tag cache
    if (buffTable && buffTable->buff_type() == kBuffTypeStealth)
    {
        tlsEcs.actorRegistry.emplace_or_replace<StealthedTagComp>(parent);
    }

    BuffImplSystem::OnBuffStart(parent, buff, buffTable);
    ModifierBuffImplSystem::OnBuffStart(parent, buff, buffTable);
    MotionModifierBuffImplSystem::OnBuffStart(parent, buff, buffTable);
}

void BuffSystem::OnBuffRefresh(entt::entity parent, uint32_t buffTableId, const SkillContextPtrComp& abilityContext, BuffEntry& buffComp)
{
    // TODO: implement buff refresh logic (e.g. reset duration, reapply modifiers)
}

void BuffSystem::OnBuffRemove(const entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable)
{
    // Maintain stealth tag cache: drop the tag only when no other stealth buff remains
    // (this buff is still in the list at this point, so exclude it from the scan).
    if (buffTable && buffTable->buff_type() == kBuffTypeStealth &&
        !HasStealthBuffExcept(parent, buffComp.buffPb.buff_id()))
    {
        tlsEcs.actorRegistry.remove<StealthedTagComp>(parent);
    }

    ModifierBuffImplSystem::OnBuffRemove(parent, buffComp, buffTable);
    MotionModifierBuffImplSystem::OnBuffRemove(parent, buffComp, buffTable);
}

void BuffSystem::OnBuffDestroy(entt::entity parent, const uint64_t buffId, const BuffTable* buffTable)
{
    BuffImplSystem::OnBuffDestroy(parent, buffId, buffTable);
}

// Buff periodic interval handler
void BuffSystem::OnIntervalThink(entt::entity parent, uint64_t buffId)
{
    // 跨实体查询一律 try_get,不用 get(CLAUDE.md §7.5):get 在组件缺失时是抛异常/UB,
    // 而这是个公开入口,调用方不保证 parent 一定有 buff 列表。
    auto *buffListPtr = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
    if (buffListPtr == nullptr) {
        return;
    }
    auto &buffList = *buffListPtr;
    const auto buffIt = buffList.find(buffId);

    if (buffIt == buffList.end()) {
        LOG_ERROR << "Cannot find buff " << buffId;
        return;
    }

    const auto buffTableId = buffIt->second.buffPb.buff_table_id();
    LookupBuffOrReturnVoid(buffTableId);

    BuffImplSystem::OnIntervalThink(parent, buffIt->second, buffRow);
    ModifierBuffImplSystem::OnIntervalThink(parent, buffIt->second, buffRow);
    MotionModifierBuffImplSystem::OnIntervalThink(parent, buffIt->second, buffRow);
}
void BuffSystem::OnSkillExecuted(SkillExecutedEvent& event)
{
    // TODO: implement buff reactions to skill execution events
}

void BuffSystem::OnBeforeGiveDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent)
{
    BuffImplSystem::OnBeforeGiveDamage(casterEntity, targetEntity, damageEvent);
}


void BuffSystem::OnAfterGiveDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent)
{
    // TODO: implement post-damage-dealt buff triggers
}

void BuffSystem::OnBeforeTakeDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent)
{
    // TODO: implement pre-damage-taken buff triggers (e.g. damage reduction shields)
}

void BuffSystem::OnAfterTakeDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent)
{
    // casterEntity is the entity that just took damage -> reset its combat-idle buff.
    BuffImplSystem::ResetCombatIdleBuff(entt::to_entity(damageEvent.attacker_id()), casterEntity);
}

void BuffSystem::OnBeforeDead(entt::entity parent)
{
    // TODO: implement pre-death buff triggers (e.g. death prevention, last stand)
}

void BuffSystem::OnAfterDead(entt::entity parent)
{
    // TODO: implement post-death buff cleanup or triggers
}

void BuffSystem::OnKill(entt::entity parent)
{
    // TODO: implement on-kill buff triggers (e.g. life steal, kill streak buffs)
}

void BuffSystem::OnSkillHit(const entt::entity casterEntity, const entt::entity targetEntity) {
    BuffImplSystem::OnSkillHit(casterEntity, targetEntity);
    ModifierBuffImplSystem::OnSkillHit(casterEntity, targetEntity);
    MotionModifierBuffImplSystem::OnSkillHit(casterEntity, targetEntity);
}

bool BuffSystem::AddSubBuffs(entt::entity parent,
    const BuffTable* buffTable,
    BuffEntry& buffComp)
{
    if (buffTable == nullptr)
    {
        return false;
    }

    // Skip if sub-buffs already added
    if (buffComp.buffPb.has_added_sub_buff()) {
        return false;
    }

    buffComp.buffPb.set_has_added_sub_buff(true);
    AddSubBuffsWithoutCheck(parent, buffTable, buffComp);
    return true;
}

void BuffSystem::AddTargetSubBuffs(const entt::entity targetEntity,
    const BuffTable* buffTable,
    const SkillContextPtrComp& abilityContext)
{
    if (buffTable == nullptr)
    {
        return;
    }

    for (auto& targetSubBuffTableId : buffTable->target_sub_buff()) {
        AddOrUpdateBuff(targetEntity, targetSubBuffTableId, abilityContext);
    }
}

void BuffSystem::AddSubBuffsWithoutCheck(entt::entity parent,
    const BuffTable* buffTable,
    BuffEntry& buffComp)
{
    // AddOrUpdateBuff 会往同一张 buffList 里插入子 buff,也可能顺着
    // DispelBuffsOnAwake 把 buffComp 自己驱散掉,所以不能端着 buffComp 的引用
    // 跨调用。记下 owner 的 id 和上下文强引用,每轮重查;owner 没了就停手。
    const auto ownerBuffId = buffComp.buffPb.buff_id();
    const SkillContextPtrComp ownerContext = buffComp.skillContext;

    for (const auto& subBuff : buffTable->sub_buff()) {
        auto [result, newBuffId] = BuffSystem::AddOrUpdateBuff(parent, subBuff, ownerContext);

        if (result != kSuccess || newBuffId == UINT64_MAX) {
            continue;
        }

        auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (buffList == nullptr) {
            return;
        }
        const auto ownerIt = buffList->find(ownerBuffId);
        if (ownerIt == buffList->end()) {
            return;
        }

        // Add to sub-buff list (false = not yet activated)
        ownerIt->second.buffPb.mutable_sub_buff_list_id()->emplace(newBuffId, false);
    }
}

bool CanApplyMoreTicks(const BuffPeriodicBuffComp& periodicBuff, const BuffTable* buffTable) {
    return (buffTable->interval_count() == 0) || (periodicBuff.ticks_done() + 1 <= buffTable->interval_count());
}

// 单个 buff 一帧最多补几次 tick(掉帧后不要一次性把攒下的 tick 全放完)。
constexpr uint32_t kMaxPeriodicTicksPerFrame = 5;

void UpdatePeriodicBuff(const entt::entity target, const uint64_t buffId, BuffListComp& buffListComp, double delta) {
    auto buffIt = buffListComp.find(buffId);
    if (buffIt == buffListComp.end()) {
        return;
    }

    LookupBuffOrReturnVoid(buffIt->second.buffPb.buff_table_id());

    if (buffRow->interval() <= 0) {
        return;
    }

    double periodicTimer = buffIt->second.buffPb.periodic().periodic_timer() + delta;

    for (uint32_t i = 0; i < kMaxPeriodicTicksPerFrame; ++i) {
        // 每一轮都重新 find:OnIntervalThink 会顺着
        // TickCombatIdleBuff -> AddSubBuffs -> AddOrUpdateBuff 往同一张 buffList
        // 里 emplace 子 buff(unordered_map 扩容),也会顺着 AddOrUpdateBuff ->
        // DispelBuffsOnAwake -> OnBuffExpire 把这条 buff 自己 erase 掉。
        // 旧写法把 BuffEntry& / periodic 的引用一路端着穿过回调,buff 被驱散后
        // 下一句 set_ticks_done 就是写已释放内存。
        buffIt = buffListComp.find(buffId);
        if (buffIt == buffListComp.end()) {
            return;  // buff 在 tick 过程中被销毁,后面什么都不能再碰
        }

        auto& periodicBuff = *buffIt->second.buffPb.mutable_periodic();
        if (!CanApplyMoreTicks(periodicBuff, buffRow) || periodicTimer < buffRow->interval()) {
            break;
        }

        periodicTimer -= buffRow->interval();
        periodicBuff.set_ticks_done(periodicBuff.ticks_done() + 1);
        // 先落盘再回调:回调里 buff 可能被销毁,那时就没机会写回了。
        periodicBuff.set_periodic_timer(periodicTimer);

        BuffSystem::OnIntervalThink(target, buffId);
    }

    buffIt = buffListComp.find(buffId);
    if (buffIt == buffListComp.end()) {
        return;
    }
    buffIt->second.buffPb.mutable_periodic()->set_periodic_timer(periodicTimer);
}

void ProcessBuffs(const entt::entity target, BuffListComp& buffListComp, const double delta) {
    // 同上:tick 回调会往这张表里增删条目,直接 range-for 遍历时持有的迭代器
    // 会在扩容 / erase 时失效。先快照 id,再逐个按 id 重查。
    UInt64Vector buffIds;
    buffIds.reserve(buffListComp.size());
    for (const auto& buffId : buffListComp | std::views::keys) {
        buffIds.emplace_back(buffId);
    }

    for (const auto buffId : buffIds) {
        UpdatePeriodicBuff(target, buffId, buffListComp, delta);
    }
}

void BuffSystem::Update(const double delta) {
    // PlayerFrozenComp exclude: buff timers must NOT keep counting down on
    // the source side while the player is mid-cross-zone-migration. The
    // marshaled PlayerAllData carries the buff list with its current
    // remaining time; if we kept ticking here a 30s buff could expire on
    // the source mid-flight and the destination would resurrect it as if
    // fresh.  cross-zone-readiness-audit.md §11.2.
    for (auto&& [target, buffListComp] : tlsEcs.actorRegistry.view<BuffListComp>(
            entt::exclude<PlayerFrozenComp>).each()) {
        ProcessBuffs(target, buffListComp, delta);
        BuffSystem::RemovePendingBuffs(target, buffListComp);
    }
}


