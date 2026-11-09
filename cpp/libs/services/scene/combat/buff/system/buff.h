#pragma once

#include "combat/buff/comp/buff_comp.h"
#include "engine/core/type_define/type_define.h"

class BuffTable;

// Caster (buff applier), Parent (buff target),
// Skill (which skill created this buff), BuffLayer (stacks),
// BuffLevel, BuffDuration,
// BuffTag, BuffImmuneTag,

class DamageEventComp;
struct BuffEntry;

// BuffListComp 是按需状态组件:「没有它」= 身上没有 buff,读路径一律 try_get,拿不到是正常分支。
// 生命周期只有两个出入口:AddOrUpdateBuff 第一次真正插入条目时创建(唯一创建入口),
// Update 在帧末发现列表为空时摘掉。除实体销毁外,其余路径都不创建、不摘除它。
class BuffSystem
{
public:
    // 每帧推进周期 buff，处理完列表为空的实体随即摘掉 BuffListComp。
    static void Update(double delta);

    // 同一帧使用同一个同进程单调毫秒读数；显式时间入口也用于确定性回归。
    static void Update(double delta, uint64_t nowMilliseconds);

    // 返回 {错误码, 新建条目的 buff_id};叠层 / 纯驱散 / 冻结或回合战斗中丢弃时 buff_id 为 UINT64_MAX。
    // 只有真正新建条目时才会在 parent 上创建 BuffListComp;被拒绝、纯驱散、叠层的施加不改变组件集合。
    static std::tuple<uint32_t, uint64_t>  AddOrUpdateBuff(entt::entity parent, uint32_t buffTableId, uint64_t caster);

    static std::tuple<uint32_t, uint64_t> AddOrUpdateBuff(entt::entity parent, uint32_t buffTableId);

    static void RemoveBuff(entt::entity parent, uint64_t buffId);

    static void RemoveBuff(entt::entity parent, const UInt64Set& removeBuffIdList);

    // 收集并清空父记录的子 Buff 号，子 Buff 实例由调用方随后移除。
    static void CollectAndClearSubBuffIds(BuffEntry& buffComp, UInt64Set& buffsToRemove);

    // 检查配置与目标免疫；返回 tip 错误码，kSuccess 表示通过。
    static uint32_t CheckBuffCreation(entt::entity parentEntity, uint32_t buffTableId);

    // 同表、同「处理后的施法者」(no_caster 时不区分施法者)的条目已存在时叠层 / 刷新并返回 true。
    // 不改变实体的组件集合;没有 BuffListComp(身上没有 buff)时返回 false。
    static bool HandleExistingBuff(entt::entity parentEntity, uint32_t buffTableId, uint64_t caster);

    // Fired after instantiation but before activation (not yet in buff container).
    // Dispels existing buffs whose tags match this buff's dispel_tag. Returns true
    // if the new buff is a pure Dispel buff and should be consumed without being
    // added to the buff list.
    static bool DispelBuffsOnAwake(entt::entity parent, uint32_t buffTableId);

    // Called when buff takes effect (added to buff container). Designer-configurable.
    static void OnBuffStart(entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable);

    // Called when a same-type/same-caster buff exists: refresh stacks, level, duration.
    static void OnBuffRefresh(entt::entity parent, uint32_t buffTableId, uint64_t caster, BuffEntry& buffComp);

    // Called before buff removal (still in buff container). Designer-configurable.
    static void OnBuffRemove(entt::entity parent, BuffEntry& buffComp, const BuffTable* buffTable);

    // Called after buff removal (already removed from container). Designer-configurable.
    static void OnBuffDestroy(entt::entity parent, const uint64_t buffId, const BuffTable* buffTable);

    // Interval-based periodic effect via StartIntervalThink. Designer-configurable.

    static void OnIntervalThink(entt::entity parent, uint64_t buffId);

    // nowMilliseconds 必须与 Buff 的 last_time 属于同一单调时钟域。
    static void OnIntervalThink(entt::entity parent, uint64_t buffId, uint64_t nowMilliseconds);

    static void OnBuffExpire(entt::entity parent, uint64_t buffId);

    // Triggered when dealing damage to target
    static void OnBeforeGiveDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent);

    // Triggered after dealing damage to target
    static void OnAfterGiveDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent);

    // Triggered before taking damage
    static void OnBeforeTakeDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent);

    // Triggered after taking damage
    static void OnAfterTakeDamage(const entt::entity casterEntity, const entt::entity targetEntity, DamageEventComp& damageEvent);

    // Triggered before self dies
    static void OnBeforeDead(entt::entity parent);

    // Triggered after self dies
    static void OnAfterDead(entt::entity parent);

    // Triggered on killing a target
    static void OnKill(entt::entity parent);

    static void OnSkillHit(entt::entity casterEntity, entt::entity targetEntity);

    static bool AddSubBuffs(
        entt::entity parent,
        const BuffTable* buffTable,
        BuffEntry& buffComp
    );

    static void AddTargetSubBuffs(
        entt::entity targetEntity,
        const BuffTable* buffTable,
        uint64_t caster
    );

    // 按配置施加子 Buff，每项仍经 AddOrUpdateBuff 正常校验；一次性触发门由 AddSubBuffs 负责。
    static void AddConfiguredSubBuffs(
        entt::entity parent,
        const BuffTable* buffTable,
        BuffEntry& buffComp
    );

private:
    // 有脱战父 id 时创建独立子项，并在启动回调前登记归属；UINT64_MAX 表示普通施加。
    static std::tuple<uint32_t, uint64_t> AddOrUpdateBuffWithOwner(
        entt::entity parent, uint32_t buffTableId,
        uint64_t caster, uint64_t combatIdleOwnerId);
};


