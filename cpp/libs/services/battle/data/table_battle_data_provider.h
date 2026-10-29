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
    // 启动时加载一次；失败不安装配置，建房拒绝缺失的 PVE 奖励档。
    bool LoadBattleRewards(const std::string& path);
    const BattleRewardProfile* FindBattleReward(uint32_t battleConfigId,
                                               const std::string& strategyKey) const override;
    const SkillTable* FindSkill(uint32_t skillTableId) const override;
    const BuffTable* FindBuff(uint32_t buffTableId) const override;
    const SkillPermissionTable* FindSkillPermission(uint32_t combatStateId) const override;
    const DungeonTable* FindDungeon(uint32_t dungeonTableId) const override;
    const MonsterTable* FindMonster(uint32_t monsterTableId) const override;
    const ItemTable* FindItem(uint32_t itemTableId) const override;

    uint64_t GetCooldownDurationMs(uint32_t cooldownTableId) const override;
    std::vector<uint32_t> GetDungeonMonsterIds(uint32_t dungeonTableId) const override;
    uint32_t GetDungeonMonstersPerPlayer(uint32_t dungeonTableId) const override;

    double GetSkillDamage(uint32_t skillTableId, double casterLevel) override;
    double GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) override;
    double GetBuffBonusDamage(uint32_t buffTableId) override;
private:
    BattleRewardConfig rewards;
};

}  // namespace turnbattle
