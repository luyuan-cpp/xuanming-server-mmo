#pragma once
#include <cstdint>

// 回合制战斗引擎常量(纯逻辑库,零网络/零 ECS 依赖,设计文档 §5.1)。
//
// 与实时战斗共享的位枚举/类型枚举的权威定义在 services/scene 下;
// 回合引擎按"不 include services/scene 头文件"的约束在此镜像数值,
// 两侧若有改动必须同步(来源文件见各枚举注释)。

namespace turnbattle {

// ---- 时间→回合换算(设计文档 §5.1 v1 策略,二期由 Excel 回合列替代) ----

// 一回合等效的挂钟毫秒数:rounds = max(1, ceil(ms / kRoundDurationMs))
inline constexpr uint64_t kRoundDurationMs = 6000;

// DungeonTable.time_limit 为 0 时的回合上限缺省值
inline constexpr uint32_t kDefaultMaxRounds = 30;

// ---- 匹配模式(镜像 proto/match/match_service.proto 的 MatchMode,决定逃跑规则等) ----

inline constexpr uint32_t kMatchModePveSolo = 4;
inline constexpr uint32_t kMatchModePveTeam = 5;

// ---- 二期:自动战斗 / 5v5 / 队伍上限(设计文档 §11,D13/D14) ----

// 每队玩家数上限(产品口径:队伍上限五个人)。引擎 Initialize 按此拒绝超编建房,
// match 侧凑单人数同步收口 min(配置值, 5),双侧强制;Dungeon 表存在 max_team_size=10
// 的历史行,以本常量为准,不改表重导
inline constexpr uint32_t kMaxBattleTeamSize = 5;

// 全自动房间(全体存活玩家均挂机)的回合推进间隔:装填回合时 AllPlayersReady()
// 立即为真的房间,节点回合 timer 用此值替代整个行动窗口——既不空转刷回合
// (观众/客户端跟得上),也不傻等行动窗口(D13)
inline constexpr uint64_t kAutoRoundIntervalMs = 2000;

// ---- 技能类型位号(镜像 cpp/libs/services/scene/combat/skill/constants/skill.h 的 eSkillType) ----
// SkillTable.skill_type 存的是位号(0..5),SkillPermission.skill_type 列按位号平铺

inline constexpr uint32_t kSkillTypeBitPassive = 0;
inline constexpr uint32_t kSkillTypeBitGeneral = 1;
inline constexpr uint32_t kSkillTypeBitChannel = 2;
inline constexpr uint32_t kSkillTypeBitToggle = 3;
inline constexpr uint32_t kSkillTypeBitActivate = 4;
inline constexpr uint32_t kSkillTypeBitBasicAttack = 5;

// ---- 目标模式位掩码(镜像同文件 eTargetRequirement;SkillTable.targeting_mode 存位号) ----

inline constexpr uint32_t kTargetingNoTargetRequired = 1 << 0;
inline constexpr uint32_t kTargetingTargetedSkill = 1 << 1;
inline constexpr uint32_t kTargetingAreaOfEffect = 1 << 2;

// ---- buff 类型(镜像 cpp/libs/services/scene/combat/buff/constants/buff.h 的 eBuffType) ----

inline constexpr uint32_t kBuffTypeStun = 30;
inline constexpr uint32_t kBuffTypeSilence = 31;
inline constexpr uint32_t kBuffTypeInvincibility = 32;
inline constexpr uint32_t kBuffTypeImmunity = 34;
inline constexpr uint32_t kBuffTypeDispel = 35;
inline constexpr uint32_t kBuffTypeHealthRegeneration = 40;
inline constexpr uint32_t kBuffTypeManaRegeneration = 41;
inline constexpr uint32_t kBuffTypeHealthRegenerationBasedOnLostHealth = 42;
inline constexpr uint32_t kBuffTypePoison = 50;
inline constexpr uint32_t kBuffTypeBurn = 51;
inline constexpr uint32_t kBuffTypeFreeze = 52;

// ---- 战斗状态(镜像 cpp/libs/services/scene/combat_state/constants/combat_state.h,
//      作为 SkillPermission 表行 id 使用) ----

inline constexpr uint32_t kCombatStateSilence = 1;

// ---- 回合规则常量(设计文档 §5.1 回合规则 v1) ----

// 普攻基础伤害:普攻无 SkillTable 行,伤害公式的 base 用此保守常量
// (建议后续在表里给普攻固定一行,见任务返回的 open_issues)
inline constexpr double kBasicAttackBaseDamage = 10.0;

// 逃跑成功率:base + 速度差 * 系数,夹在 [min, max](PVE 可逃,PVP 一期不可逃)
inline constexpr double kFleeBaseChance = 0.5;
inline constexpr double kFleeSpeedFactor = 0.01;
inline constexpr double kFleeMinChance = 0.05;
inline constexpr double kFleeMaxChance = 0.95;

// 战斗道具 v1 保守效果:固定回血(ItemTable 未接入回合引擎,见 open_issues)
inline constexpr uint64_t kDefaultItemHealHp = 100;

// 子 buff 递归深度上限(防表配环)
inline constexpr uint32_t kMaxSubBuffDepth = 8;

// ---- 怪物侧保守默认值(MonsterTable 目前只有 id 列,缺属性列,见 open_issues) ----

inline constexpr uint64_t kMonsterActorIdBase = 1000000;  // 怪物局内 actor_id 起始值
inline constexpr uint64_t kMonsterDefaultHealth = 300;
inline constexpr uint64_t kMonsterDefaultStrength = 5;
inline constexpr uint64_t kMonsterDefaultArmor = 2;
inline constexpr uint64_t kMonsterDefaultResistance = 0;
inline constexpr uint64_t kMonsterDefaultCritChance = 0;
inline constexpr uint64_t kMonsterDefaultSpeed = 5;

// 时间(毫秒)换算回合:rounds = max(1, ceil(ms / kRoundDurationMs))
inline constexpr uint32_t RoundsFromMilliseconds(uint64_t durationMs) {
    const uint64_t rounds = (durationMs + kRoundDurationMs - 1) / kRoundDurationMs;
    return rounds < 1 ? 1u : static_cast<uint32_t>(rounds);
}

// 时间(秒,Excel double 口径,与 muduo RunAfter 一致)换算回合
inline constexpr uint32_t RoundsFromSeconds(double durationSeconds) {
    if (durationSeconds <= 0) {
        return 1;
    }
    return RoundsFromMilliseconds(static_cast<uint64_t>(durationSeconds * 1000.0));
}

}  // namespace turnbattle
