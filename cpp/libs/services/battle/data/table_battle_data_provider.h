#pragma once
#include <memory>

#include "data/battle_data_provider.h"

// 生产环境数据供给实现:读全局表管理器
// (SkillTableManager/BuffTableManager/CooldownTableManager/SkillPermissionTableManager/
//  DungeonTableManager/MonsterTableManager)。
// battle 节点启动时须完成各表 Load,再创建引擎。

namespace turnbattle {

class TableBattleDataProvider final : public BattleDataProvider {
public:
    const SkillTable* FindSkill(uint32_t skillTableId) const override;
    const BuffTable* FindBuff(uint32_t buffTableId) const override;
    const SkillPermissionTable* FindSkillPermission(uint32_t combatStateId) const override;
    const DungeonTable* FindDungeon(uint32_t dungeonTableId) const override;
    const MonsterTable* FindMonster(uint32_t monsterTableId) const override;

    uint64_t GetCooldownDurationMs(uint32_t cooldownTableId) const override;
    std::vector<uint32_t> GetDungeonMonsterIds(uint32_t dungeonTableId) const override;

    double GetSkillDamage(uint32_t skillTableId, double casterLevel) override;
    double GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) override;
    double GetBuffBonusDamage(uint32_t buffTableId) override;
};

// 生产实现工厂:引擎默认构造走这里,把表管理器依赖隔离在单独编译单元
std::shared_ptr<BattleDataProvider> MakeTableBattleDataProvider();

}  // namespace turnbattle
