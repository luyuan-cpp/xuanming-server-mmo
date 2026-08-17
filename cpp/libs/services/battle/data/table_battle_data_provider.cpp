#include "data/table_battle_data_provider.h"

#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/cooldown_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"

namespace turnbattle {

const SkillTable* TableBattleDataProvider::FindSkill(uint32_t skillTableId) const {
    // Silent 版:查不到不刷错误日志,是否算错误由引擎按调用场景判定
    return SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
}

const BuffTable* TableBattleDataProvider::FindBuff(uint32_t buffTableId) const {
    return BuffTableManager::Instance().FindByIdSilent(buffTableId).first;
}

const SkillPermissionTable* TableBattleDataProvider::FindSkillPermission(uint32_t combatStateId) const {
    return SkillPermissionTableManager::Instance().FindByIdSilent(combatStateId).first;
}

const DungeonTable* TableBattleDataProvider::FindDungeon(uint32_t dungeonTableId) const {
    return DungeonTableManager::Instance().FindByIdSilent(dungeonTableId).first;
}

const MonsterTable* TableBattleDataProvider::FindMonster(uint32_t monsterTableId) const {
    return MonsterTableManager::Instance().FindByIdSilent(monsterTableId).first;
}

uint64_t TableBattleDataProvider::GetCooldownDurationMs(uint32_t cooldownTableId) const {
    // CooldownTable.duration 毫秒口径,与实时战斗 CoolDownTimeMillisecondSystem 一致
    const auto* row = CooldownTableManager::Instance().FindByIdSilent(cooldownTableId).first;
    return row == nullptr ? 0 : row->duration();
}

std::vector<uint32_t> TableBattleDataProvider::GetDungeonMonsterIds(uint32_t dungeonTableId) const {
    // DungeonTable 目前没有怪物组列(仅 id/scene_id/max_team_size/time_limit),
    // 返回空表 → 引擎按保守默认值生成怪物侧;等 Excel 加列后在此接入
    (void)dungeonTableId;
    return {};
}

double TableBattleDataProvider::GetSkillDamage(uint32_t skillTableId, double casterLevel) {
    // damage 表达式两步调用:先 SetDamageParam({casterLevel}) 再 GetDamage,
    // 与实时战斗 CalculateSkillDamage 的用法一致
    SkillTableManager::Instance().SetDamageParam({casterLevel});
    return SkillTableManager::Instance().GetDamage(skillTableId);
}

double TableBattleDataProvider::GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) {
    // 镜像 ModifierBuffImplSystem::OnHealthRegenerationBasedOnLostHealth 的参数口径
    BuffTableManager::Instance().SetHealthRegenerationParam({level, lostHealth});
    return BuffTableManager::Instance().GetHealthRegeneration(buffTableId);
}

double TableBattleDataProvider::GetBuffBonusDamage(uint32_t buffTableId) {
    return BuffTableManager::Instance().GetBonusDamage(buffTableId);
}

std::shared_ptr<BattleDataProvider> MakeTableBattleDataProvider() {
    return std::make_shared<TableBattleDataProvider>();
}

}  // namespace turnbattle
