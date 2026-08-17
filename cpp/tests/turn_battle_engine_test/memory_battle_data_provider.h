#pragma once
#include <map>
#include <vector>

#include "data/battle_data_provider.h"

// 单测用内存数据供给:直接喂内存表行,隔离 Excel 数据与表管理器加载流程。
// 行指针指向 std::map 节点(节点稳定),生命周期覆盖整个测试用例。
//
// 表达式类(伤害/回血/附加伤害)在单测里退化为固定数值表:
// 生产实现的两步表达式调用(SetDamageParam+GetDamage)由 TableBattleDataProvider 覆盖,
// 引擎侧只关心求值结果。

class MemoryBattleDataProvider final : public turnbattle::BattleDataProvider {
public:
    // ---- 数据装配(测试用例在 Initialize 前填好) ----

    SkillTable& AddSkill(uint32_t skillTableId) {
        auto& row = skillRows[skillTableId];
        row.set_id(skillTableId);
        return row;
    }

    BuffTable& AddBuff(uint32_t buffTableId) {
        auto& row = buffRows[buffTableId];
        row.set_id(buffTableId);
        return row;
    }

    SkillPermissionTable& AddSkillPermission(uint32_t combatStateId) {
        auto& row = permissionRows[combatStateId];
        row.set_id(combatStateId);
        return row;
    }

    DungeonTable& AddDungeon(uint32_t dungeonTableId) {
        auto& row = dungeonRows[dungeonTableId];
        row.set_id(dungeonTableId);
        return row;
    }

    MonsterTable& AddMonster(uint32_t monsterTableId) {
        auto& row = monsterRows[monsterTableId];
        row.set_id(monsterTableId);
        return row;
    }

    void SetCooldownMs(uint32_t cooldownTableId, uint64_t durationMs) {
        cooldownDurations[cooldownTableId] = durationMs;
    }

    void SetDungeonMonsters(uint32_t dungeonTableId, std::vector<uint32_t> monsterIds) {
        dungeonMonsterIds[dungeonTableId] = std::move(monsterIds);
    }

    void SetSkillDamage(uint32_t skillTableId, double damage) {
        skillDamageValues[skillTableId] = damage;
    }

    void SetBuffRegen(uint32_t buffTableId, double regen) {
        buffRegenValues[buffTableId] = regen;
    }

    void SetBuffBonusDamage(uint32_t buffTableId, double bonus) {
        buffBonusValues[buffTableId] = bonus;
    }

    // ---- BattleDataProvider 实现 ----

    const SkillTable* FindSkill(uint32_t skillTableId) const override {
        const auto it = skillRows.find(skillTableId);
        return it == skillRows.end() ? nullptr : &it->second;
    }

    const BuffTable* FindBuff(uint32_t buffTableId) const override {
        const auto it = buffRows.find(buffTableId);
        return it == buffRows.end() ? nullptr : &it->second;
    }

    const SkillPermissionTable* FindSkillPermission(uint32_t combatStateId) const override {
        const auto it = permissionRows.find(combatStateId);
        return it == permissionRows.end() ? nullptr : &it->second;
    }

    const DungeonTable* FindDungeon(uint32_t dungeonTableId) const override {
        const auto it = dungeonRows.find(dungeonTableId);
        return it == dungeonRows.end() ? nullptr : &it->second;
    }

    const MonsterTable* FindMonster(uint32_t monsterTableId) const override {
        const auto it = monsterRows.find(monsterTableId);
        return it == monsterRows.end() ? nullptr : &it->second;
    }

    uint64_t GetCooldownDurationMs(uint32_t cooldownTableId) const override {
        const auto it = cooldownDurations.find(cooldownTableId);
        return it == cooldownDurations.end() ? 0 : it->second;
    }

    std::vector<uint32_t> GetDungeonMonsterIds(uint32_t dungeonTableId) const override {
        const auto it = dungeonMonsterIds.find(dungeonTableId);
        return it == dungeonMonsterIds.end() ? std::vector<uint32_t>{} : it->second;
    }

    double GetSkillDamage(uint32_t skillTableId, double casterLevel) override {
        (void)casterLevel;  // 单测固定数值,不做等级缩放
        const auto it = skillDamageValues.find(skillTableId);
        return it == skillDamageValues.end() ? 0.0 : it->second;
    }

    double GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) override {
        (void)level;
        (void)lostHealth;
        const auto it = buffRegenValues.find(buffTableId);
        return it == buffRegenValues.end() ? 0.0 : it->second;
    }

    double GetBuffBonusDamage(uint32_t buffTableId) override {
        const auto it = buffBonusValues.find(buffTableId);
        return it == buffBonusValues.end() ? 0.0 : it->second;
    }

private:
    std::map<uint32_t, SkillTable> skillRows;
    std::map<uint32_t, BuffTable> buffRows;
    std::map<uint32_t, SkillPermissionTable> permissionRows;
    std::map<uint32_t, DungeonTable> dungeonRows;
    std::map<uint32_t, MonsterTable> monsterRows;
    std::map<uint32_t, uint64_t> cooldownDurations;
    std::map<uint32_t, std::vector<uint32_t>> dungeonMonsterIds;
    std::map<uint32_t, double> skillDamageValues;
    std::map<uint32_t, double> buffRegenValues;
    std::map<uint32_t, double> buffBonusValues;
};
