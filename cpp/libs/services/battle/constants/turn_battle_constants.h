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

// ---- 表现规格(docs/design/turn-battle-presentation.md §2 D1-D5)相关常量 ----

// 命中判定基础命中率(百分比)。一期 Skill/Monster 表均无命中率/闪避列,
// 引擎按本常量走判定骨架:命中率 >= 100 时不消耗随机数、永不产出 MISS,
// 既有同种子回放基线不受影响;二期接表(SkillTable 命中列 / MonsterTable 闪避列)后
// 由 RollHit 在此基础上减闪避,届时新增的随机数消费位于伤害暴击掷骰之前,
// 需同步刷新回放基线(见 turn_battle_engine_test.cpp 的说明)
inline constexpr uint32_t kBaseHitRate = 100;

// SkillTable.cost_resource[].cost_resource_id 中表示"法力"的资源 id
// (Skill.xlsx 第 1 行示例为 {id=1,cost=10},id=2 语义未定,引擎只消费 id=1;
// 其余资源 id 一期忽略,见任务 open_issues)
inline constexpr uint32_t kSkillCostResourceMana = 1;

// 阵位:每队 0..4 为前排(左→右),5..9 为后排(D4)。队伍上限 5 人时玩家只占前排;
// 怪物按副本怪物组顺序落位,组内超过 5 只落后排
inline constexpr uint32_t kFormationFrontRowSize = 5;

// ---- 怪物侧保守默认值(仅在 MonsterTable 查不到行/属性缺失时回退,2026-09-02 起
//      正常从 MonsterTable 读属性;兜底怪 monster_table_id=0 走这些常量) ----

// ---- 局内 actor_id 命名空间 ----
// 玩家单位直接用 player_id;非玩家单位(怪物 / 宝宝)用引擎局内序号,放进 bit63 打标的保留段。
//
// 为什么必须隔开(2026-09-11):ID 号段改造后,player_id 与 item/宠物 id 都由 data_service
// 从 1 起递增发号。旧写法两处都会撞:
//   - 宝宝直接拿 pet_id 当 actor_id:1 号玩家带着 1 号宝宝就同号,InitPets 查重后拒绝开局;
//   - 怪物用 1000000 + 序号:第 100 万个玩家进 PVE 就与 0 号怪同号,而怪物追加**不查重**,
//     伤害/目标按 actor_id 找人时会静默打错单位。
// player_id 两个来源都 < 2^63(号段从 1 递增;遗留 bwmarrin SnowFlake 是正的 int64),
// 且 InitPlayers 对 bit63 置位的 player_id 直接 fail-closed —— 这是被强制的不变量,不是假设。
inline constexpr uint64_t kEngineLocalActorIdFlag = 1ULL << 63;
inline constexpr uint64_t kMonsterActorIdBase = kEngineLocalActorIdFlag | (1ULL << 32);  // 怪物局内 actor_id 起始值
inline constexpr uint64_t kPetActorIdBase = kEngineLocalActorIdFlag | (2ULL << 32);      // 宝宝局内 actor_id 起始值
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
