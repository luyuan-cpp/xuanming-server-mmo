#include "buff.h"
#include <chrono>
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
#include "core/utils/utility/utility.h"
#include "player/comp/player_frozen_comp.h" // Frozen exclude — cross-zone-readiness-audit.md §11.2
#include "proto/common/component/battle_comp.pb.h" // InBattleComp:回合制战斗局中闸
#include "core/system/id_generator.h"
#include "spatial/system/aoi.h" // RefreshTargetVisibility:隐身状态变化后重判 AOI 观察者
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

namespace {

uint64_t SteadyNowMilliseconds()
{
    return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
        std::chrono::steady_clock::now().time_since_epoch()).count());
}

// 「处理后的施法者」:新建条目时写进 processed_caster,叠层时按它找同一条 buff。
// 新建与叠层必须走这同一条规则,否则两边算出来的值永远对不上。
// - no_caster 的 buff 不区分施法者:一律记 kNoBuffCaster,任何施法者再挂都叠到同一条上;
// - 其余按施法者隔离;没有施法者的系统施加同样记 kNoBuffCaster。
// 写进去的取值与回合引擎 TurnBattleEngine::StackOrRefreshExistingBuff 的 caster_id 一致,
// 但叠层行为并不完全一致:按施法者隔离、又没有施法者的施加,实时侧仍每次新建一条
// (HandleExistingBuff 直接返回 false),回合引擎会把 caster_id==0 的条目互相叠层。
// 这是有意保留的差异 —— 子 buff 链路(同一 owner 挂多条同表子 buff)依赖它,要对齐须另行决策。
// 以前 no_caster 写的是 entt::null(uint64 全 1),叠层时却拿它和施法者整数比较,永远不等:
// 每挂一次就新建一条,max_layer 形同虚设(C003)。
uint64_t ProcessedCasterOf(const BuffTable& buffRow, uint64_t caster)
{
    return buffRow.no_caster() != 0 ? kNoBuffCaster : caster;
}

bool IsCombatIdleBonus(const BuffListComp& buffList, uint64_t childId)
{
    for (const auto& owner : buffList | std::views::values) {
        if (!owner.buffPb.sub_buff_list_id().contains(childId)) {
            continue;
        }
        LookupBuffOrContinue(owner.buffPb.buff_table_id());
        if (buffRow->buff_type() == kBuffTypeNoDamageOrSkillHitInLastSeconds) {
            return true;
        }
    }
    return false;
}

} // namespace

BuffMessagePtr CreateBuffDataPtr(const BuffTable* buffTable) {
    switch (buffTable->buff_type()) {
    case kBuffTypeNoDamageOrSkillHitInLastSeconds: {
        auto data = std::make_shared<BuffNoDamageOrSkillHitInLastSecondsComp>();
        // 从首次获得父 Buff 开始等待，不要求先受击。dataPbPtr 目前仅用于运行时，
        // last_time 是同进程单调毫秒，不能当作 Unix 时间或跨进程快照恢复。
        data->set_last_time(SteadyNowMilliseconds());
        return data;
    }
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

// 按施法者整数施加或叠层 Buff。
std::tuple<uint32_t, uint64_t> BuffSystem::AddOrUpdateBuff(
    const entt::entity parent,
    const uint32_t buffTableId,
    uint64_t caster)
{
    return AddOrUpdateBuffWithOwner(parent, buffTableId, caster, UINT64_MAX);
}

std::tuple<uint32_t, uint64_t> BuffSystem::AddOrUpdateBuffWithOwner(
    const entt::entity parent,
    const uint32_t buffTableId,
    uint64_t caster,
    const uint64_t combatIdleOwnerId)
{
    if (!tlsEcs.actorRegistry.valid(parent))
    {
        return {kThisEntityIsInvalid, UINT64_MAX};
    }

    // 冻结中的目标(归属交接在途:跨 zone 传送 / 跨节点换图):buff 列表已经在冻结那一刻的
    // 存盘里打进 PlayerAllData,接手的节点会照快照重建。源端此时再加 buff,只会造出一条
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

    // 回合制战斗在途:实时 buff 不落到玩家身上。局内 buff 是引擎自己的一套
    // (快照只带走开战那一刻的),此刻在场景里挂的 buff 战斗内看不见、战后还在,
    // 等于凭空多出一个"战斗期间获得的增益"。
    if (tlsEcs.actorRegistry.any_of<InBattleComp>(parent))
    {
        LOG_INFO << "[TurnBattle] Buff on in-battle target dropped. parent="
                 << entt::to_integral(parent) << " buff_table=" << buffTableId;
        return {kSuccess, UINT64_MAX};
    }

    LookupBuffOrReturn(buffTableId, (std::make_tuple(buffResult, UINT64_MAX)));

    if (const auto result = CanCreateBuff(parent, buffTableId); result != kSuccess) {
        return {result, UINT64_MAX};
    }

    // Pure dispel buffs and stack/refresh of an existing buff don't create a new entry.
    if (DispelBuffsOnAwake(parent, buffTableId)) {
        return {kSuccess, UINT64_MAX};
    }

    // 持续加成必须能随各自的父独立撤销，不能叠进其他父或独立施加的条目。
    if (combatIdleOwnerId == UINT64_MAX && HandleExistingBuff(parent, buffTableId, caster)) {
        return {kSuccess, UINT64_MAX};
    }

    // 驱散会经 OnBuffExpire 跑一串回调。它们不销毁实体,但对已销毁的实体 emplace 是未定义行为,
    // 落到下面的延迟创建之前复核一次。
    if (!tlsEcs.actorRegistry.valid(parent)) {
        LOG_ERROR << "AddOrUpdateBuff: parent destroyed during dispel callbacks. parent="
                  << entt::to_integral(parent) << " buff_table=" << buffTableId;
        return {kThisEntityIsInvalid, UINT64_MAX};
    }

    if (combatIdleOwnerId != UINT64_MAX) {
        const auto* liveList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (liveList == nullptr || !liveList->contains(combatIdleOwnerId)) {
            return {kSuccess, UINT64_MAX};  // 子项的驱散已移除了脱战父，不再创建孤儿
        }
    }

    // 此处延迟创建:BuffListComp 是按需状态(没有它 = 身上没有 buff),这里是它唯一的创建入口,
    // 帧末 Update 发现列表空了再摘掉。放在免疫 / 驱散 / 叠层判断之后 —— 被拒绝、纯驱散、
    // 叠层的施加都不会给实体凭空挂上一张空表(F037);以前在驱散判断之前就 get_or_emplace。
    auto& buffList = tlsEcs.actorRegistry.get_or_emplace<BuffListComp>(parent);

    BuffEntry newBuff;
    newBuff.buffPb.set_caster(caster);
    newBuff.buffPb.set_processed_caster(ProcessedCasterOf(*buffRow, caster));
    // 新条目就是第 1 层,叠层上限 max_layer 才是「最多几层」(与回合引擎新建条目的 layer=1 一致)。
    // 以前从 0 起算,max_layer=N 实际能叠到 N+1 次。
    newBuff.buffPb.set_layer(1);

    uint64_t newBuffId = GenerateUniqueBuffId(buffList);
    newBuff.buffPb.set_buff_id(newBuffId);
    newBuff.buffPb.set_buff_table_id(buffTableId);
    newBuff.dataPbPtr = CreateBuffDataPtr(buffRow);

    auto [fst, snd] = buffList.emplace(newBuffId, std::move(newBuff));
    if (combatIdleOwnerId != UINT64_MAX) {
        // 子项插入时同时登记归属,让普通叠层与父移除看到完整父子关系。
        buffList.at(combatIdleOwnerId).buffPb.mutable_sub_buff_list_id()->emplace(newBuffId, false);
    }
    OnBuffStart(parent, fst->second, buffRow);

    // 显式无限时长优先于 duration，同时保留旧配置 duration<0 的永久语义。
    if (buffRow->infinite_duration() != 0) {
        return {kSuccess, newBuffId};
    }
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

// 系统施加，不指定施法者。
std::tuple<uint32_t, uint64_t> BuffSystem::AddOrUpdateBuff(
    const entt::entity parent,
    const uint32_t buffTableId)
{
    return AddOrUpdateBuff(parent, buffTableId, kNoBuffCaster);
}


// Remove buff
void BuffSystem::RemoveBuff(const entt::entity parent, const uint64_t buffId)
{
    OnBuffExpire(parent, buffId);
}

void BuffSystem::RemoveBuff(const entt::entity parent, const UInt64Set& removeBuffIdList) {
    // 调用方可以传入实体上的待删集合,先拷贝以免级联清理使遍历失效。
    const auto removeIds = removeBuffIdList;
    for (const auto removeBuffId : removeIds) {
        // 前一条的级联清理可能已经移除了本条，批量移除按仍存活的 id 处理。
        const auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (buffList == nullptr) {
            return;
        }
        if (buffList->contains(removeBuffId)) {
            BuffSystem::RemoveBuff(parent, removeBuffId);
        }
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

    UInt64Set bonusSubBuffs;
    if (buffRow->buff_type() == kBuffTypeNoDamageOrSkillHitInLastSeconds) {
        // 脱战加成以父 Buff 存活为前提；父被驱散或到期后，永久子 Buff 也必须撤销。
        BuffSystem::RemoveSubBuff(buffIt->second, bonusSubBuffs);
    }
    OnBuffRemove(parent, buffIt->second, buffRow);
    buffList.erase(buffId);
    for (auto& liveBuff : buffList | std::views::values) {
        liveBuff.buffPb.mutable_sub_buff_list_id()->erase(buffId);
    }
    OnBuffDestroy(parent, buffId, buffRow);
    // 先摘父再移除子，防止子回调再次驱散父而重复进入同一条记录。
    BuffSystem::RemoveBuff(parent, bonusSubBuffs);
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
    uint64_t caster)
{
    LookupBuffOrReturnFalse(buffTableId);

    // 按施法者隔离却没有施法者时，保持每次新建一条的既有行为。
    // no_caster 的 buff 不看施法者，照常叠层。
    if (buffRow->no_caster() == 0 && caster == kNoBuffCaster) {
        return false;
    }

    // 叠层判断只找已有条目,不改实体的组件集合:BuffListComp 是按需状态,没有它就是身上
    // 没有 buff,自然没有可叠的。以前这里 get_or_emplace,一次判断就给实体挂上一张空表(F037)。
    auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parentEntity);
    if (buffList == nullptr) {
        return false;
    }

    const auto processedCaster = ProcessedCasterOf(*buffRow, caster);
    for (auto& buffComp : *buffList | std::views::values) {
        if (buffComp.buffPb.buff_table_id() == buffTableId && buffComp.buffPb.processed_caster() == processedCaster) {
            if (IsCombatIdleBonus(*buffList, buffComp.buffPb.buff_id())) {
                continue;
            }
            if (buffComp.buffPb.layer() < buffRow->max_layer()) {
                buffComp.buffPb.set_layer(buffComp.buffPb.layer() + 1);
            }
            OnBuffRefresh(parentEntity, buffTableId, caster, buffComp);
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

    // Maintain stealth tag cache. 标签从无到有时让 AOI 立即重判观察者:没有侦测授权的
    // 观察者暂停投递并收到一次移除(docs/design/aoi-directed-interest.md「可见与同步」)。
    if (buffTable && buffTable->buff_type() == kBuffTypeStealth &&
        !tlsEcs.actorRegistry.any_of<StealthedTagComp>(parent))
    {
        tlsEcs.actorRegistry.emplace<StealthedTagComp>(parent);
        AoiSystem::RefreshTargetVisibility(parent);
    }

    BuffImplSystem::OnBuffStart(parent, buff, buffTable);
    ModifierBuffImplSystem::OnBuffStart(parent, buff, buffTable);
    MotionModifierBuffImplSystem::OnBuffStart(parent, buff, buffTable);
}

void BuffSystem::OnBuffRefresh(entt::entity parent, uint32_t buffTableId, uint64_t caster, BuffEntry& buffComp)
{
    // TODO: implement buff refresh logic (e.g. reset duration, reapply modifiers)
}

void BuffSystem::OnBuffRemove(const entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable)
{
    // Maintain stealth tag cache: drop the tag only when no other stealth buff remains
    // (this buff is still in the list at this point, so exclude it from the scan).
    // 标签真正摘掉时,暂停的显式关注恢复、附近能看见它的实体重新建立空间关注,都先收到当前状态。
    if (buffTable && buffTable->buff_type() == kBuffTypeStealth &&
        !HasStealthBuffExcept(parent, buffComp.buffPb.buff_id()) &&
        tlsEcs.actorRegistry.remove<StealthedTagComp>(parent) > 0)
    {
        AoiSystem::RefreshTargetVisibility(parent);
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
    OnIntervalThink(parent, buffId, SteadyNowMilliseconds());
}

void BuffSystem::OnIntervalThink(entt::entity parent, uint64_t buffId, uint64_t nowMilliseconds)
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

    // 三路回调之间不能端着 buffIt / BuffEntry& 一路传下去,每一路之前都按 id 重查:
    // BuffImplSystem 会顺着 TickCombatIdleBuff -> AddSubBuffs -> AddOrUpdateBuff 往同一张
    // buffList 里 emplace 子 buff(unordered_map 扩容,迭代器失效),子 buff 的
    // DispelBuffsOnAwake -> OnBuffExpire 还可能把这条 buff 自己 erase 掉(引用悬空)。
    // 与 UpdatePeriodicBuff 每轮重查同理;buff 在前一路里没了,后面几路就不再回调。
    using IntervalThinkHandler = void (*)(entt::entity, BuffEntry&, const BuffTable*, uint64_t);
    constexpr IntervalThinkHandler kIntervalThinkHandlers[] = {
        &BuffImplSystem::OnIntervalThink,
        [](entt::entity actor, BuffEntry& buff, const BuffTable* row, uint64_t) {
            ModifierBuffImplSystem::OnIntervalThink(actor, buff, row);
        },
        [](entt::entity actor, BuffEntry& buff, const BuffTable* row, uint64_t) {
            MotionModifierBuffImplSystem::OnIntervalThink(actor, buff, row);
        },
    };
    for (const auto handler : kIntervalThinkHandlers) {
        auto *liveBuffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (liveBuffList == nullptr) {
            return;
        }
        const auto liveIt = liveBuffList->find(buffId);
        if (liveIt == liveBuffList->end()) {
            return;  // 前一路回调里被驱散 / 移除,后面什么都不能再碰
        }
        handler(parent, liveIt->second, buffRow, nowMilliseconds);
    }
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
    // 受击方是 targetEntity(唯一调用方 skill.cpp TriggerAfterDamageEvents 传的是 攻击者, 受击者)。
    // 以前误把 casterEntity 当受击者,重置的是攻击者自己的脱战加成;攻击者也不必再从
    // damageEvent.attacker_id 绕一圈 uint64 → entity 取回。
    BuffImplSystem::ResetCombatIdleBuff(casterEntity, targetEntity, SteadyNowMilliseconds());
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
    BuffImplSystem::OnSkillHit(casterEntity, targetEntity, SteadyNowMilliseconds());
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
    uint64_t caster)
{
    if (buffTable == nullptr)
    {
        return;
    }

    for (auto& targetSubBuffTableId : buffTable->target_sub_buff()) {
        AddOrUpdateBuff(targetEntity, targetSubBuffTableId, caster);
    }
}

void BuffSystem::AddSubBuffsWithoutCheck(entt::entity parent,
    const BuffTable* buffTable,
    BuffEntry& buffComp)
{
    // AddOrUpdateBuff 会往同一张 buffList 里插入子 buff,也可能顺着
    // DispelBuffsOnAwake 把 buffComp 自己驱散掉,所以不能端着 buffComp 的引用
    // 跨调用。记下 owner 的 id 和施法者整数，每轮重查；owner 没了就停手。
    const auto ownerBuffId = buffComp.buffPb.buff_id();
    const uint64_t ownerCaster = buffComp.buffPb.caster();
    const bool isCombatIdleOwner = buffTable->buff_type() == kBuffTypeNoDamageOrSkillHitInLastSeconds;

    for (const auto& subBuff : buffTable->sub_buff()) {
        const auto* before = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (before == nullptr || !before->contains(ownerBuffId)) {
            return;
        }
        auto [result, newBuffId] = isCombatIdleOwner
            ? AddOrUpdateBuffWithOwner(parent, subBuff, ownerCaster, ownerBuffId)
            : AddOrUpdateBuff(parent, subBuff, ownerCaster);

        auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(parent);
        if (buffList == nullptr) {
            return;
        }
        const auto ownerIt = buffList->find(ownerBuffId);
        if (ownerIt == buffList->end()) {
            // 子 Buff 在创建时反向驱散父：不留下无人负责撤销的永久加成。
            if (isCombatIdleOwner && result == kSuccess && newBuffId != UINT64_MAX && buffList->contains(newBuffId)) {
                BuffSystem::RemoveBuff(parent, newBuffId);
            }
            return;
        }

        // 瞬时子 Buff 已在 AddOrUpdateBuff 内到期，不能把它登记成活跃子项。
        if (result != kSuccess || newBuffId == UINT64_MAX || !buffList->contains(newBuffId)) {
            continue;
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

void UpdatePeriodicBuff(const entt::entity target, const uint64_t buffId, BuffListComp& buffListComp,
                        double delta, uint64_t nowMilliseconds) {
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

        BuffSystem::OnIntervalThink(target, buffId, nowMilliseconds);
    }

    buffIt = buffListComp.find(buffId);
    if (buffIt == buffListComp.end()) {
        return;
    }
    buffIt->second.buffPb.mutable_periodic()->set_periodic_timer(periodicTimer);
}

void ProcessBuffs(const entt::entity target, BuffListComp& buffListComp, const double delta, uint64_t nowMilliseconds) {
    // 同上:tick 回调会往这张表里增删条目,直接 range-for 遍历时持有的迭代器
    // 会在扩容 / erase 时失效。先快照 id,再逐个按 id 重查。
    UInt64Vector buffIds;
    buffIds.reserve(buffListComp.size());
    for (const auto& buffId : buffListComp | std::views::keys) {
        buffIds.emplace_back(buffId);
    }

    for (const auto buffId : buffIds) {
        UpdatePeriodicBuff(target, buffId, buffListComp, delta, nowMilliseconds);
    }
}

void BuffSystem::Update(const double delta) {
    Update(delta, SteadyNowMilliseconds());
}

void BuffSystem::Update(const double delta, uint64_t nowMilliseconds) {
    // PlayerFrozenComp exclude:归属交接在途(PlayerLifecycleSystem::StartTravelHandoff:
    // 跨 zone 传送 / 同 zone 跨节点换图)的玩家,源端不再推进 buff 计时。交接的前提是
    // "冻结那一次存盘之后本端内存态不再变":放行后实体由 DestroyDeposedPlayer 不存盘销毁,
    // 这里继续 tick 只是白做;未成则 AbortTravelHandoff 解冻,从冻结时的剩余时间接着走。
    // 已没有 Kafka 迁移包 / ACK / reaper。被动 tick 排除目录见
    // cross-zone-readiness-audit.md §11.2(该文只有 §11 仍有效)。
    for (auto&& [target, buffListComp] : tlsEcs.actorRegistry.view<BuffListComp>(
            entt::exclude<PlayerFrozenComp>).each()) {
        ProcessBuffs(target, buffListComp, delta, nowMilliseconds);

        // 按需状态的结束:列表空了就摘掉 BuffListComp(之后 buffListComp 引用失效,不能再碰)。
        // 只在帧末这里摘:buff 到期 / 驱散发生在各种回调深处,调用栈上还可能有人端着这张表的引用
        // (比如 ProcessBuffs / UpdatePeriodicBuff 拿的就是本循环的 buffListComp),当场摘会让它们悬空。
        // EnTT 允许遍历中移除「当前实体」的被遍历组件。
        if (buffListComp.empty()) {
            tlsEcs.actorRegistry.remove<BuffListComp>(target);
        }
    }
}


