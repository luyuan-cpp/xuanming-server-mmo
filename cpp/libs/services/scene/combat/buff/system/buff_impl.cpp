#include "buff_impl.h"

#include <vector>

#include "table/code/buff_table.h"
#include "combat/buff/system/buff.h"
#include "combat_state/constants/combat_state.h"
#include "combat/buff/constants/buff.h"
#include "macros/return_define.h"
#include "combat_state/system/combat_state.h"
#include "core/utils/defer/defer.h"

void BuffImplSystem::OnIntervalThink(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable,
                                    uint64_t nowMilliseconds) {
    if (buffTable && buffTable->buff_type() == kBuffTypeNoDamageOrSkillHitInLastSeconds) {
        TickCombatIdleBuff(parent, buff, buffTable, nowMilliseconds);
    }
}

void BuffImplSystem::OnBuffStart(entt::entity parent, const BuffEntry& buff, const BuffTable* buffTable) {
    if (buffTable && buffTable->buff_type() == kBuffTypeSilence) {
        AddSilenceState(parent, buff);
    }
}

void BuffImplSystem::OnBuffDestroy(entt::entity parent, uint64_t buffId, const BuffTable* buffTable) {
    if (buffTable && buffTable->buff_type() == kBuffTypeSilence) {
        RemoveSilenceState(parent, buffId);
    }
}

void BuffImplSystem::OnBeforeGiveDamage(entt::entity caster, entt::entity target, DamageEventComp& damageEvent) {
    // Apply any "next basic attack" bonus the caster is holding, then drop those buffs.
    UInt64Set buffsToRemove;
    defer(BuffSystem::RemoveBuff(caster, buffsToRemove));

    // 先收集、再逐个按 id 重查:ApplyNextBasicAttackBonus 会经 AddSubBuffs → AddOrUpdateBuff
    // 往同一张 BuffListComp(unordered_map)里插入子 buff(可能 rehash),也可能顺着
    // DispelBuffsOnAwake 删掉条目 —— 边遍历边改会让 range-for 的迭代器失效。
    std::vector<uint64_t> bonusBuffIds;
    {
        ECS_GET_OR_VOID(buffList, BuffListComp, caster);
        for (const auto& [buffId, buff] : *buffList) {
            LookupBuffOrContinue(buff.buffPb.buff_table_id());
            if (buffRow->buff_type() == kBuffTypeNextBasicAttack) {
                bonusBuffIds.push_back(buffId);
            }
        }
    }

    for (const uint64_t buffId : bonusBuffIds) {
        ECS_GET_OR_VOID(buffList, BuffListComp, caster);
        const auto buffIt = buffList->find(buffId);
        if (buffIt == buffList->end()) {
            continue;  // 已被前一条加成带出的子 buff 驱散
        }
        LookupBuffOrContinue(buffIt->second.buffPb.buff_table_id());
        ApplyNextBasicAttackBonus(caster, target, buffIt->second, buffRow, damageEvent, buffsToRemove);
    }
}

void BuffImplSystem::OnSkillHit(entt::entity caster, entt::entity target, uint64_t nowMilliseconds) {
    ResetCombatIdleBuff(caster, target, nowMilliseconds);
}

void BuffImplSystem::ResetCombatIdleBuff(entt::entity caster, entt::entity victim, uint64_t nowMilliseconds) {
    // 清理加成后允许下一次安静窗口重新授予；同一轮安静期间仍只授予一次。
    UInt64Set buffsToRemove;
    defer(BuffSystem::RemoveBuff(victim, buffsToRemove));

    ECS_GET_OR_VOID(buffList, BuffListComp, victim);
    for (auto& buff : *buffList | std::views::values) {
        LookupBuffOrContinue(buff.buffPb.buff_table_id());
        if (buffRow->buff_type() != kBuffTypeNoDamageOrSkillHitInLastSeconds) {
            continue;
        }
        if (const auto data = std::dynamic_pointer_cast<BuffNoDamageOrSkillHitInLastSecondsComp>(buff.dataPbPtr)) {
            data->set_last_time(nowMilliseconds);
        }
        BuffSystem::CollectAndClearSubBuffIds(buff, buffsToRemove);
        buff.buffPb.set_has_added_sub_buff(false);
    }
}

void BuffImplSystem::AddSilenceState(entt::entity parent, const BuffEntry& buff) {
    CombatStateSystem::AddCombatState(parent, buff.buffPb.buff_id(), kActorCombatStateSilence);
}

void BuffImplSystem::RemoveSilenceState(entt::entity parent, uint64_t buffId) {
    CombatStateSystem::RemoveCombatState(parent, buffId, kActorCombatStateSilence);
}

void BuffImplSystem::TickCombatIdleBuff(entt::entity parent, BuffEntry& buff, const BuffTable* buffTable,
                                      uint64_t nowMilliseconds) {
    if (buffTable->combat_idle_seconds() <= 0) {
        return;
    }

    const auto data = std::dynamic_pointer_cast<BuffNoDamageOrSkillHitInLastSecondsComp>(buff.dataPbPtr);
    if (!data || nowMilliseconds < data->last_time()) {
        return;
    }

    // 配表单位为秒，时钟单位为毫秒；达到阈值才授予，等于阈值也算完成等待。
    const double idleSeconds = static_cast<double>(nowMilliseconds - data->last_time()) / 1000.0;
    if (idleSeconds < buffTable->combat_idle_seconds()) {
        return;
    }

    BuffSystem::AddSubBuffs(parent, buffTable, buff);
}

void BuffImplSystem::ApplyNextBasicAttackBonus(entt::entity caster, entt::entity target, BuffEntry& buff,
                                               const BuffTable* buffTable, DamageEventComp& damageEvent,
                                               UInt64Set& buffsToRemove) {
    const auto bonusDamage = BuffTableManager::Instance().GetBonusDamage(buffTable->id());
    damageEvent.set_damage(damageEvent.damage() + bonusDamage);

    buffsToRemove.emplace(buff.buffPb.buff_id());

    // AddSubBuffs 之后 buff 可能已被驱散删除，先复制施法者整数。
    const uint64_t sourceCaster = buff.buffPb.caster();
    BuffSystem::AddSubBuffs(caster, buffTable, buff);
    BuffSystem::AddTargetSubBuffs(target, buffTable, sourceCaster);
}
