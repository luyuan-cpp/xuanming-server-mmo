#pragma once
#include <cstdint>
#include <vector>

#include "table/proto/skill_table.pb.h"
#include "table/proto/buff_table.pb.h"
#include "table/proto/skillpermission_table.pb.h"
#include "table/proto/dungeon_table.pb.h"
#include "table/proto/monster_table.pb.h"

// 回合制战斗引擎的表数据供给接口(设计文档 §5.1)。
//
// 引擎只经由本接口读配置:生产实现(TableBattleDataProvider)读全局表管理器,
// 单测实现喂内存数据,使引擎单测不依赖 Excel 数据与表加载流程。
// 返回的行指针生命周期由实现方保证覆盖整场战斗(表管理器快照/测试内存表均满足)。

namespace turnbattle {

class BattleDataProvider {
public:
    virtual ~BattleDataProvider() = default;

    // ---- 行查询:查不到返回 nullptr,引擎侧视为配置缺失 ----

    virtual const SkillTable* FindSkill(uint32_t skillTableId) const = 0;
    virtual const BuffTable* FindBuff(uint32_t buffTableId) const = 0;
    virtual const SkillPermissionTable* FindSkillPermission(uint32_t combatStateId) const = 0;
    virtual const DungeonTable* FindDungeon(uint32_t dungeonTableId) const = 0;
    virtual const MonsterTable* FindMonster(uint32_t monsterTableId) const = 0;

    // 冷却时长(毫秒;CooldownTable.duration 与实时战斗同口径),查不到返回 0
    virtual uint64_t GetCooldownDurationMs(uint32_t cooldownTableId) const = 0;

    // 副本怪物组:DungeonTable 目前缺怪物组列,生产实现返回空表,
    // 引擎按保守默认值生成怪物侧(建议 Excel 加列,见任务 open_issues)
    virtual std::vector<uint32_t> GetDungeonMonsterIds(uint32_t dungeonTableId) const = 0;

    // ---- 表达式求值:镜像表管理器 SetXxxParam + GetXxx 两步调用 ----

    // 技能伤害:参数 {casterLevel},对应 SkillTableManager::SetDamageParam + GetDamage
    virtual double GetSkillDamage(uint32_t skillTableId, double casterLevel) = 0;

    // buff 回血:参数 {level, lostHealth},对应 BuffTableManager::SetHealthRegenerationParam
    // + GetHealthRegeneration(镜像 ModifierBuffImplSystem 的调用方式)
    virtual double GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) = 0;

    // buff 附加伤害:对应 BuffTableManager::GetBonusDamage
    virtual double GetBuffBonusDamage(uint32_t buffTableId) = 0;
};

}  // namespace turnbattle
