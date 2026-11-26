#pragma once
#include <memory>
#include <string>
#include <vector>

#include "data/battle_data_provider.h"

struct TableSet;

// 生产环境数据供给实现(docs/design/config-table-hot-reload.md §8.4):
// 显式持有一套不可变 TableSet 与共享 BattleRewardConfig,全程从所绑定 TableSet 读取,
// 不在战斗过程中读全局表管理器单例。同一套表共用一个 provider;房间持有 provider,
// 这一套表就保活到整局结束。
namespace turnbattle {

class TableBattleDataProvider final : public BattleDataProvider {
public:
    TableBattleDataProvider();
    TableBattleDataProvider(std::shared_ptr<const TableSet> tableSet,
                            std::shared_ptr<const BattleRewardConfig> sharedRewards);

    // 启动时加载一次奖励配置;失败不安装配置,建房拒绝缺失的 PVE 奖励档。
    bool LoadBattleRewards(const std::string& path);
    std::shared_ptr<const BattleRewardConfig> SharedRewards() const { return rewards; }

    // 热更校验用:用现役 rewards 核对候选 TableSet 里的 Item 表是否仍满足所有随机掉落引用。
    static bool ValidateRewardsAgainst(const BattleRewardConfig& rewardConfig,
                                       const TableSet& candidate,
                                       std::string& err);

    const std::shared_ptr<const TableSet>& BoundTableSet() const { return tableSet; }

    const BattleRewardProfile* FindBattleReward(uint32_t battleConfigId,
                                                const std::string& strategyKey) const override;
    const SkillTable* FindSkill(uint32_t skillTableId) const override;
    const BuffTable* FindBuff(uint32_t buffTableId) const override;
    const SkillPermissionTable* FindSkillPermission(uint32_t combatStateId) const override;
    const DungeonTable* FindDungeon(uint32_t dungeonTableId) const override;
    const MonsterTable* FindMonster(uint32_t monsterTableId) const override;
    const ItemTable* FindItem(uint32_t itemTableId) const override;
    const PetTable* FindPet(uint32_t petTableId) const override;
    std::vector<uint32_t> GetBanEligibleSkillIds(uint32_t activeSeasonId) const override;

    uint64_t GetCooldownDurationMs(uint32_t cooldownTableId) const override;
    std::vector<uint32_t> GetDungeonMonsterIds(uint32_t dungeonTableId) const override;
    uint32_t GetDungeonMonstersPerPlayer(uint32_t dungeonTableId) const override;

    double GetSkillDamage(uint32_t skillTableId, double casterLevel) override;
    double GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) override;
    double GetBuffBonusDamage(uint32_t buffTableId) override;

private:
    std::shared_ptr<const TableSet> tableSet;
    std::shared_ptr<const BattleRewardConfig> rewards;
};

}  // namespace turnbattle
