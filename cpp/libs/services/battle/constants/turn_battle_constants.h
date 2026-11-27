#pragma once
#include <cstddef>
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

// 手动回合收集指令的等待上限,与技能/buff 的时间→回合换算独立。
inline constexpr uint64_t kActionTimeoutMs = 25000;
// 首领试炼 V2 手动指令窗收紧至 18 秒，减少多人干等（boss-trial-planning-ui.md §3.1）。
inline constexpr uint64_t kBossTrialActionTimeoutMs = 18000;
// 论剑 V3 战前 15 秒明牌变阵窗口（仅正式自养论剑与赛季锦标赛开启，全员锁定或到期立即进第 1 回合）。
inline constexpr uint64_t kTacticalPrepTimeoutMs = 15000;
inline constexpr uint32_t kTacticalPrepDurationSeconds = 15;
// 战前技能禁选（每方 3 Ban · 方案 A + V2 每方 1 护招破禁）：开启禁选的对局变阵窗延长至 25 秒（全员锁定仍立即开打）。
inline constexpr uint32_t kPrepSkillBanSlotsPerTeam = 3;
inline constexpr uint32_t kPrepSkillProtectSlotsPerTeam = 1;
inline constexpr uint64_t kPrepProtectHashSalt = 0x50524F54454354ULL;
inline constexpr uint64_t kTacticalPrepWithBanTimeoutMs = 25000;
inline constexpr uint32_t kTacticalPrepWithBanDurationSeconds = 25;

// DungeonTable.time_limit 为 0 时的回合上限缺省值
inline constexpr uint32_t kDefaultMaxRounds = 300;

// ---- 匹配模式(镜像 proto/match/match_service.proto 的 MatchMode,决定逃跑规则等) ----

inline constexpr uint32_t kMatchModePveSolo = 4;
inline constexpr uint32_t kMatchModePveTeam = 5;
inline constexpr uint32_t kMatchModePolarity5v5 = 8;
inline constexpr uint32_t kMatchModeGuildWarPvp = 9;
inline constexpr uint32_t kMatchModeGuildWarSiege = 10;
inline constexpr uint32_t kMatchModeSeasonTournament = 11;
inline constexpr uint32_t kMatchModeJianghuDuel = 12;

// 天墉生死状默认 30 回合上限（打满双方均存活权威判平）。
inline constexpr uint32_t kJianghuDuelDefaultMaxRounds = 30;

// 帮会全天帮战 V2（docs/design/guild-play-v2.md）：
// 第 12 回合起启用「决胜煞气」：每层增伤 +15%、疗盾 -15%（下限 20%），20 回合打满判平。
inline constexpr uint32_t kGuildWarDefaultMaxRounds = 20;
inline constexpr uint32_t kGuildWarEscalationStartRound = 12;
inline constexpr uint32_t kGuildWarEscalationDamageStepPct = 15;
inline constexpr uint32_t kGuildWarEscalationSupportStepPct = 15;

// 论剑 / 竞技 PVP V2 体验规则（docs/design/ranked-play-v2-overhaul.md）：
// 1. 主目标凝锋：竞技 PVP 人物多目标直伤技在目标不足上限时，显式主目标伤害 +20%（恢复 1V1/残局集火博弈）；
// 2. 战意渐盛：回合上限 >= 10 的竞技 PVP 自 maxRounds - 7 起每回合增伤 +10%、疗盾 -10%（下限 20%）；
// 3. PVP 延迟投降：第 5 回合解锁，1V1 秒认输，多人需全部活跃队友同意（连续 >=2 回合未手动挂机者不阻挡）。
inline constexpr uint64_t kPvpPrimaryFocusNum = 120;
inline constexpr uint64_t kPvpPrimaryFocusDen = 100;
inline constexpr uint32_t kPvpEscalationMinMaxRounds = 10;
inline constexpr uint32_t kPvpEscalationLeadRounds = 7;
inline constexpr uint32_t kPvpEscalationDamageStepPct = 10;
inline constexpr uint32_t kPvpEscalationSupportStepPct = 10;
inline constexpr uint32_t kPvpEscalationSupportFloorPct = 20;
inline constexpr uint32_t kPvpSurrenderUnlockRound = 5;
inline constexpr uint32_t kPvpSurrenderRejectCooldownRounds = 3;
inline constexpr uint32_t kPvpSurrenderExpireCooldownRounds = 2;
inline constexpr uint32_t kPvpSurrenderIdleExcludeRounds = 2;
// 论剑 V4 队内一键战术标点、本局复盘高光徽记与连胜破势门槛（纯确定性整数规则）。
inline constexpr uint32_t kMaxBattlePingsPerRound = 2;
inline constexpr uint32_t kMinWinStreakHighlight = 3;
inline constexpr uint32_t kClutchComebackMinEscalationStacks = 2;
inline constexpr uint32_t kClutchComebackLowHpDenominator = 4; // hp * 4 < max_hp (< 25%)
inline constexpr uint32_t kHighlightControlMinCount = 2;
// 论剑 V5 临阵唤灵：正式自养竞技第 2 回合起解锁，每人整局严格限 1 次唤出备选灵宠。
inline constexpr uint32_t kPvpSummonPetUnlockRound = 2;

// 赛季首领试炼与轮换娱乐玩法类型（对应 DungeonTable.boss_trial_kind）
inline constexpr uint32_t kBossTrialKindNone = 0;
inline constexpr uint32_t kBossTrialKindBreakRejuv = 1;      // 破阵·回春（慢速重击 + 回春护卫）
inline constexpr uint32_t kBossTrialKindBreakStance = 2;     // 破阵·护势（慢速重击 + 护势护卫）
inline constexpr uint32_t kBossTrialKindBarrier = 3;         // 灵障首领（灵障护盾 + 强法重击）
inline constexpr uint32_t kBossTrialKindVenom = 4;           // 玄毒首领（缚灵封控 + 群体玄毒）
inline constexpr uint32_t kBossTrialKindGuardianRescue = 5;  // 护卫救援（8回合保护友方护卫击败首领）
inline constexpr uint32_t kBossTrialKindPolarity5v5 = 6;     // 物法易势 5V5（每2回合切换物理/法术+15%，16回合平局）
inline constexpr uint32_t kBossTrialKindFestivalBeast = 7;   // 瑞兽迎春（单人携宠12回合，R2/5/8/11预告、R3/6/9/12慢速180%物理重击）
inline constexpr uint32_t kBossTrialKindBreakDualGuard = 8;  // 破阵·双卫同阵（困难终极合阵：回春+护势同场交织）
inline constexpr uint32_t kBossTrialKindPracticeDummy = 9;   // 归元城演武试招木桩（强制本人养成、满血蓝隔离、零战损零资源产出）
inline constexpr uint32_t kBossTrialKindTacticalPuzzle = 10; // 归元城演武回合残局考题（支持本人养成或免费预设、零刷取隔离）
inline constexpr uint32_t kBossTrialKindCharacterTale = 11;  // 归元城人物奇谈短案（支持本人养成或免费预设试演、零刷取隔离）
inline constexpr uint32_t kBossTrialKindWildWind = 12;       // 卷四·八景岛风眼引风明雷首领
inline constexpr uint32_t kBossTrialKindWildBell = 13;       // 卷四·碧渊水宫水镜钟鸣明雷首领
inline constexpr uint32_t kBossTrialKindWildTide = 14;       // 卷四·月汐湾涨退潮明雷首领

constexpr bool IsWildBossTrialKind(uint32_t kind) {
    return kind >= kBossTrialKindWildWind && kind <= kBossTrialKindWildTide;
}

constexpr bool IsBossTrialKind(uint32_t kind) {
    return (kind >= kBossTrialKindBreakRejuv && kind <= kBossTrialKindVenom) ||
           kind == kBossTrialKindBreakDualGuard ||
           IsWildBossTrialKind(kind);
}

// 卷四三境明雷周期窗口（整数确定性）：
// - 碧渊水宫：每 2 回合交替阳钟（R1-2/5-6/9-10，物理 +15%/法术 -12%）与阴鼓（R3-4/7-8/11-12，法术 +15%/物理 -12%）。
// - 月汐湾：每 2 回合交替涨潮（R1-2/5-6/9-10，首领承伤 -25%）与退潮（R3-4/7-8/11-12，首领承伤 +20%）。
constexpr bool IsWildBellYangRound(uint32_t roundIndex) {
    const uint32_t block = (roundIndex > 0u ? (roundIndex - 1u) : 0u) / 2u;
    return (block % 2u) == 0u;
}

constexpr bool IsWildTideEbbRound(uint32_t roundIndex) {
    const uint32_t block = (roundIndex > 0u ? (roundIndex - 1u) : 0u) / 2u;
    return (block % 2u) == 1u;
}

constexpr bool IsPracticeDummyKind(uint32_t kind) {
    return kind == kBossTrialKindPracticeDummy;
}

constexpr bool IsNpcIsolatedEncounterKind(uint32_t kind) {
    return kind >= kBossTrialKindPracticeDummy && kind <= kBossTrialKindCharacterTale;
}

constexpr bool AllowsOptionalFreePresetKind(uint32_t kind) {
    return kind == kBossTrialKindTacticalPuzzle || kind == kBossTrialKindCharacterTale;
}

constexpr bool IsBossTrialOrRescueKind(uint32_t kind) {
    return IsBossTrialKind(kind) || kind == kBossTrialKindGuardianRescue;
}

constexpr bool IsBossStanceTrialKind(uint32_t kind) {
    return kind == kBossTrialKindBreakStance || kind == kBossTrialKindBreakDualGuard;
}

constexpr bool IsBossRejuvTrialKind(uint32_t kind) {
    return kind == kBossTrialKindBreakRejuv || kind == kBossTrialKindBreakDualGuard;
}

// 首领试炼 V2 护势联防阵类型（对应 BattleStateS2C.boss_stance_type：0=无，1=物防阵，2=法防阵；
// 不可直接存 combatdamage::kMagicDamage=0，否则法防阵与“无护势”冲突）。
inline constexpr uint32_t kBossStanceNone = 0;
inline constexpr uint32_t kBossStancePhysical = 1;
inline constexpr uint32_t kBossStanceMagic = 2;

// 首领试炼 V2 怪物表角色定位 ID（对应 data/Monster.xlsx，不修改既有 101..108）
inline constexpr uint32_t kBossTrialMonsterGuardRejuv = 102;
inline constexpr uint32_t kBossTrialMonsterGuardStance = 103;
inline constexpr uint32_t kBossTrialMonsterSupportBreaker = 109; // 策应卫·破军
inline constexpr uint32_t kBossTrialMonsterSupportDrummer = 110; // 策应卫·鼓噪

// 首领试炼 V2 核心数值标尺（整数百分点，全平台确定性）
inline constexpr uint64_t kBossTrialSplashKeepPct = 60;      // 多目标直伤副目标溅射保留 60%（减伤 40%）
inline constexpr uint32_t kBossExposedFullPct = 35;          // 完整破绽易伤 +35%
inline constexpr uint32_t kBossExposedPartialPct = 20;       // 裂地双劈半化解轻破绽 +20%
inline constexpr uint32_t kBossPhaseTwoVulnPct = 15;         // 二阶段决战姿态自身易伤 +15%（与破绽加算）
inline constexpr uint32_t kBossMomentumMaxStacks = 3;        // 蓄势上限 3 层
inline constexpr uint32_t kBossMomentumPerStackPct = 12;     // 每层蓄势使重劈增伤 +12%
inline constexpr uint64_t kBossHealBacklashPct = 6;          // 断疗反噬扣除首领 6% 最大气血
inline constexpr uint64_t kBossDrummerDeathShockPct = 4;     // 鼓噪卫阵亡震荡扣除首领 4% 最大气血
inline constexpr uint64_t kBossDualGuardHpKeepPct = 75;      // 双卫同阵两位核心护卫气血压缩至 75%

// BATTLE_EVENT_BOSS_MECHANIC 机制子事件（对应 BattleEventItem.mechanic_kind）
inline constexpr uint32_t kBossMechanicCleaveParryExpose = 1; // 化劲破绽（value=35 或 20）
inline constexpr uint32_t kBossMechanicHealBacklash = 2;      // 断疗反噬（value=反噬扣血）
inline constexpr uint32_t kBossMechanicStanceBreak = 3;       // 崩势破盾（value=35）
inline constexpr uint32_t kBossMechanicSupportShock = 4;      // 阵脚震荡（value=清蓄势数或震荡扣血）
inline constexpr uint32_t kBossMechanicPhaseTwoEnrage = 5;    // 决战姿态切换（value=2）
inline constexpr uint32_t kBossMechanicMomentumGain = 6;      // 蓄势叠加（value=当前层数）
inline constexpr uint32_t kBossMechanicBreakerSunder = 7;     // 破军破甲（value=20）
inline constexpr uint32_t kBossMechanicRejuvCleanse = 8;      // 困难回春净益（value=清除项数）
inline constexpr uint32_t kBossMechanicAntiHealDishearten = 9;// 断脉压制气馁（value=15）
inline constexpr uint32_t kBossMechanicWeeklyAffixTrigger = 10;// 周常五行词缀机制触发（木周回蓝/回血、土周赐盾人数）

// 预告动作种类（对应 BattleTelegraphEntry.intent_kind）
inline constexpr uint32_t kTelegraphKindSlowCleave = 1;
inline constexpr uint32_t kTelegraphKindGuardHeal = 2;
inline constexpr uint32_t kTelegraphKindGuardStancePhys = 3;
inline constexpr uint32_t kTelegraphKindGuardStanceMagic = 4;
inline constexpr uint32_t kTelegraphKindGuardShield = 5;
inline constexpr uint32_t kTelegraphKindGuardBind = 6;
inline constexpr uint32_t kTelegraphKindBossMagicStrike = 7;
inline constexpr uint32_t kTelegraphKindBossVenomAoe = 8;
inline constexpr uint32_t kTelegraphKindFestivalHeavyStrike = 9;
inline constexpr uint32_t kTelegraphKindWildWindGust = 10;

// 预告状态（对应 BattleTelegraphEntry.status）
inline constexpr uint32_t kTelegraphStatusUpcoming = 1;
inline constexpr uint32_t kTelegraphStatusExecuting = 2;
inline constexpr uint32_t kTelegraphStatusExecuted = 3;
inline constexpr uint32_t kTelegraphStatusCancelledByControl = 4;
inline constexpr uint32_t kTelegraphStatusCancelledTargetInvalid = 5;

// 物法易势默认回合上限与同系直接主伤害放大比例（+15%，整数向下取整一次）
inline constexpr uint32_t kPolarityDefaultMaxRounds = 16;
inline constexpr uint64_t kPolarityDamageNum = 115;
inline constexpr uint64_t kPolarityDamageDen = 100;

// ---- 二期:自动战斗 / 5v5 / 队伍上限(设计文档 §11,D13/D14) ----

// 每队玩家数上限(产品口径:队伍上限五个人)。引擎 Initialize 按此拒绝超编建房,
// match 侧凑单人数同步收口 min(配置值, 5),双侧强制;Dungeon 表存在 max_team_size=10
// 的历史行,以本常量为准,不改表重导
inline constexpr uint32_t kMaxBattleTeamSize = 5;

// 全自动房间(全体存活玩家均挂机)的回合推进间隔**下限**:装填回合时 AllPlayersReady()
// 立即为真的房间,节点回合 timer 不等整个行动窗口——既不空转刷回合
// (观众/客户端跟得上),也不傻等行动窗口(D13)。实际间隔见 AutoRoundIntervalMsFor
inline constexpr uint64_t kAutoRoundIntervalMs = 2000;

// 全自动间隔按刚结算那一回合出手的单位数放宽(pve-team-size-matching.md §2.2)。客户端要在这个窗口里
// 把那一回合演完:每个出手单位约 0.9s、压缩上限 6 倍、收尾余量 0.3s(客户端 PlaybackBudget),
// 固定 2000ms 只容得下约 11 个单位;PVE 按人数放大后一局最多 5 人 + 5 宠 + 10 怪,不放宽的话
// 客户端会把整回合演出跳过。每单位 250ms:8 个单位以内仍是 2000ms(既有节奏不变),
// 15 个 3750ms,20 个 5000ms
inline constexpr uint64_t kAutoRoundPerActorMs = 250;

// actedActorCount = 刚结算那一回合排定出手的单位数(TurnBattleEngine::LastActionOrder().size());
// 开局首次装填还没有回合可演,传 0 即得下限
constexpr uint64_t AutoRoundIntervalMsFor(std::size_t actedActorCount) {
    const uint64_t scaledMs = static_cast<uint64_t>(actedActorCount) * kAutoRoundPerActorMs;
    return scaledMs > kAutoRoundIntervalMs ? scaledMs : kAutoRoundIntervalMs;
}

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
inline constexpr uint32_t kBuffTypeNextBasicAttack = 36;
inline constexpr uint32_t kBuffTypeHealthRegeneration = 40;
inline constexpr uint32_t kBuffTypeManaRegeneration = 41;
inline constexpr uint32_t kBuffTypeHealthRegenerationBasedOnLostHealth = 42;
inline constexpr uint32_t kBuffTypePoison = 50;
inline constexpr uint32_t kBuffTypeBurn = 51;
inline constexpr uint32_t kBuffTypeFreeze = 52;

// 属性增强类(同文件 eBuffType 10..13)。四职业技能的增减益借这三种类型标明"改哪一项",
// 幅度不读 Buff 表,读局内 BattleBuffEntry.stat_percent(施加时按施法者技能等级算定,带符号,负 = 减益)
inline constexpr uint32_t kBuffTypeIncreaseAttack = 10;
inline constexpr uint32_t kBuffTypeIncreaseDefense = 11;
inline constexpr uint32_t kBuffTypeMovementSpeedBoost = 13;

// 四职业技能增减益作用的属性(docs/design/class-skills.md §6.4)
enum class ClassBuffStat : uint8_t {
    kNone = 0,
    kPhysicalAttack,  // 物伤:普攻与物理技能取攻击时乘上
    kDefense,         // 防御:受击方的减伤公式里乘上
    kSpeed,           // 速度:回合开始排出手序时乘上(所以本回合施加的从下一回合起生效)、逃跑判定同口径
    kCount,
};

// buff_type → 作用属性;不是这三种类型的返回 kNone(老 buff 一律不按比例改属性)
constexpr ClassBuffStat ClassSkillStatOfBuffType(uint32_t buffType) {
    switch (buffType) {
    case kBuffTypeIncreaseAttack:
        return ClassBuffStat::kPhysicalAttack;
    case kBuffTypeIncreaseDefense:
        return ClassBuffStat::kDefense;
    case kBuffTypeMovementSpeedBoost:
        return ClassBuffStat::kSpeed;
    default:
        return ClassBuffStat::kNone;
    }
}

// 同一属性增减叠加后的倍率下限:减益再多,属性也只降到原值的 10%,不归零(归零会让伤害公式与出手序退化)
inline constexpr double kMinClassBuffStatMultiplier = 0.1;

// ---- 职业玩家挂机选技的基础阈值(docs/design/class-skills.md §6.5) ----
// 都是单位当前值 / 上限的比例。危急治疗不含端点(正好 30% 不算危急),其余三项含端点。
// 这是当前基础 AI 的取值,不是战斗规则:只影响挂机玩家选哪一手,出手校验不读它们
inline constexpr double kAutoCriticalHealRatio = 0.30;       // 友方气血低于它:危急治疗
inline constexpr double kAutoHealRatio = 0.70;               // 友方气血不高于它:常规治疗
inline constexpr double kAutoRestoreManaRatio = 0.30;        // 目标法力不高于它:回蓝
inline constexpr double kAutoDefenseBuffHealthRatio = 0.60;  // 目标气血不高于它:防御增益

// ---- 战斗状态(镜像 cpp/libs/services/scene/combat_state/constants/combat_state.h,
//      作为 SkillPermission 表行 id 使用) ----

inline constexpr uint32_t kCombatStateSilence = 1;

// ---- 回合规则常量(设计文档 §5.1 回合规则 v1) ----

// 普攻基础伤害:普攻无 SkillTable 行,伤害公式的 base 用此保守常量
// (建议后续在表里给普攻固定一行,见任务返回的 open_issues)
inline constexpr double kBasicAttackBaseDamage = 10.0;

// PVP 伤害系数(2026-09-13 用户选定):非 PVE 对局里玩家与宝宝的直接伤害(普攻 / 技能)再乘本值。
// 加点百分比公式 + 比例减伤口径下,同级同配置 1v1 普攻一下就接近或超过对方气血上限(首击秒杀)。
// 2026-09-14 删相性 / 仙魔后重新标定 0.2 → 0.3:0.2 是按含相性土相的伤害标的,删掉后同级通用 1v1 在
// 7 / 10 / 30 / 60 / 85 级要 15 / 13 / 9 / 8 / 7 下;0.3 为 10 / 8 / 6 / 5 / 5 下(全暴击 5 / 5 / 3 / 3 / 3),
// 贴近原定"30 级以上约 5 下、全暴击 3~4 下"(推算见 player-attribute-allocation.md §8)。
// 毒 / 灼烧等周期伤害是表里的固定值,不乘。
inline constexpr double kPvpDamageScale = 0.3;

// 逃跑成功率:base + 速度差 * 系数,夹在 [min, max](PVE 可逃,PVP 一期不可逃)
// 速度单位 2026-09-14 起 ×12(让敏捷每分配 1 点面板至少 +1 速度),系数同除 12:
// 同样的相对速度差给出与改前相同的成功率。这是全仓唯一把速度差绝对值换成概率的地方
inline constexpr double kFleeBaseChance = 0.5;
inline constexpr double kFleeSpeedFactor = 0.01 / 12.0;
inline constexpr double kFleeMinChance = 0.05;
inline constexpr double kFleeMaxChance = 0.95;

// 战斗道具效果读 ItemTable 的 battle_usable / battle_heal_hp / battle_heal_mp 三列
// (2026-09-17 加列,取代原先写死的 kDefaultItemHealHp=100)。

// 掉落概率分母:MonsterTable.drop 的 drop_rate 是万分比整数(10000 = 必掉)。
// 用整数概率而不是浮点,是为了让策划填的值在任何平台上掷出同一结果
inline constexpr uint32_t kDropRateDenominator = 10000;

// PVP 每人每场道具使用上限。PVE 不限次。
// 理由:打满 max_rounds 判进攻方负(§5.1),不限次用药的防守方可以靠海量药水拖满回合白赢;
// 同时 PVP 一期不可逃,进攻方没有止损手段。数值可按战斗节奏调,调了要同步改设计文档
inline constexpr uint32_t kMaxItemUsesPerBattlePvp = 5;

// 子 buff 递归深度上限(防表配环)
inline constexpr uint32_t kMaxSubBuffDepth = 8;

// ---- 战斗类属性(装备加成,docs/design/equipment-attributes.md §4.5,D3–D8) ----
//
// 数值来自 BattleActorState.combat(玩家快照带入;怪物 / 宝宝全 0),百分比一律是整数百分点。
//
// 随机数消费顺序(确定性铁律:调换任何两步 = 平移全部同种子回放,必须同步刷新单测基线):
//   普攻 / 单体技能选目标:只有目标失效需要重选时才掷一次 RandIndex,排在该次行动的最前;
//   普攻的每一段:命中(RollHit)→ 必杀 → 落伤害 → 反震 → 连击续段判定;
//   整次普攻(含连击追加段)结束后:反击判定 → 反击那一下的 命中 → 必杀 → 落伤害;
//   技能的每个目标:命中 → 必杀 → 落伤害 → effect[] 逐个 buff 的抗异常判定。
// 普攻这几步的先后由单测 CombatFractionalRollsFollowDocumentedRandomOrder 用独立随机源逐步对账
// (概率取 0 / 100 的用例两端都不掷骰,看不出顺序被调换)。
// 连击 / 反击 / 反震 / 抗异常的概率判定走 TurnBattleEngine::RollPercent:概率 <= 0 恒不成立、
// >= 100 恒成立,两端都不消耗随机数(与 RollHit 的短路口径一致),只有 1..99 才掷一次 Rand01。
// 所以属性全 0 的单位(怪物、宝宝、无装备玩家)的事件流与本功能落地前逐位一致。
// 必杀是唯一的例外:它沿用落地前的写法(合计暴击率 > 0 即掷骰,满 100 也掷),
// 改成两端短路会平移存量回放里所有满暴击单位之后的随机序列。

// 必杀伤害倍率(D6):物理 / 法术必杀共用,沿用引擎落地前写死的 ×2
inline constexpr double kCriticalDamageMultiplier = 2.0;

// 连击(D3):普攻命中后按出手者 combo_rate 追加的段数上限,以及追加段的伤害百分比
// (追加段先按普攻公式独立结算、可独立必杀,再乘这个百分比;同样可以触发目标的反震)。
// 只挂普攻(含技能校验失败降级成的普攻),技能不连击
inline constexpr uint32_t kMaxComboExtraHits = 1;
inline constexpr uint32_t kComboDamagePercent = 50;

// 反震(D5):受到普攻伤害(含连击段)且仍存活时,按受击者 reflect_rate 把该段实扣伤害的这个百分比
// 弹回出手者。弹回的伤害不吃减伤、不必杀、不受防御指令影响,至少 1 点
inline constexpr uint32_t kReflectDamagePercent = 50;
// 引擎用"先拆商和余数再乘"的整数算法求 ceil(实扣 × 百分比 / 100),百分比超过 100 时那个算法会溢出
static_assert(kReflectDamagePercent <= 100);

// 抗异常(D8):带异常状态的 buff 类型 → 目标身上对应的单项抗性(CombatAttributes 的 resist_* 字段)。
// 有效抵抗率 = max(0, 单项 + resist_all_ailment − 施加者.ignore_ailment_resist),夹到 100。
// 引擎没有与问道同名的「昏睡」「遗忘」状态,按效果就近映射:昏睡 → 眩晕(无法行动),遗忘 → 沉默(放不出技能)。
// 「混乱」在引擎里没有对应的 buff 类型,所以 resist_confusion 暂无消费点(只贯通到面板与战斗快照);
// 以后加了混乱 buff,在枚举与下表各补一项即可。灼烧(51)不在问道的异常清单里,不判抵抗。
enum class AilmentResistKind : uint8_t {
    kPoison = 0,  // CombatAttributes.resist_poison
    kFreeze,      // CombatAttributes.resist_freeze
    kSleep,       // CombatAttributes.resist_sleep(昏睡)
    kForget,      // CombatAttributes.resist_forget(遗忘)
    kCount,
};

struct AilmentBuffMapping {
    uint32_t buffType;             // BuffTable.buff_type
    AilmentResistKind resistKind;  // 该类型吃哪一项单项抗性
};

inline constexpr AilmentBuffMapping kAilmentBuffMappings[] = {
    {kBuffTypePoison, AilmentResistKind::kPoison},
    {kBuffTypeFreeze, AilmentResistKind::kFreeze},
    {kBuffTypeStun, AilmentResistKind::kSleep},
    {kBuffTypeSilence, AilmentResistKind::kForget},
};
// 每项单项抗性恰有一个 buff 类型消费它:枚举加了项而表没跟上(或反过来)在这里编译失败
static_assert(sizeof(kAilmentBuffMappings) / sizeof(kAilmentBuffMappings[0]) ==
              static_cast<std::size_t>(AilmentResistKind::kCount));

// ---- 表现规格(docs/design/turn-battle-presentation.md §2 D1-D5)相关常量 ----

// 命中判定基础命中率(百分比)。一期 Skill/Monster 表均无命中率/闪避列,
// 引擎按本常量走判定骨架:命中率 >= 100 时不消耗随机数、永不产出 MISS,
// 既有同种子回放基线不受影响;二期接表(SkillTable 命中列 / MonsterTable 闪避列)后
// 由 RollHit 在此基础上减闪避,届时新增的随机数消费位于伤害暴击掷骰之前,
// 需同步刷新回放基线(见 turn_battle_engine_test.cpp 的说明)
inline constexpr uint32_t kBaseHitRate = 100;

// SkillTable.cost_resource[].cost_resource_id 中表示"法力"的资源 id
// (Skill.xlsx 第 1 行为 {id=1,cost=40}(2026-09-14 法力单位 ×4,原 10),id=2 语义未定,引擎只消费 id=1;
// 其余资源 id 一期忽略,见任务 open_issues)
inline constexpr uint32_t kSkillCostResourceMana = 1;

// 阵位:每队 0..4 为前排(左→右),5..9 为后排(D4)。队伍上限 5 人时玩家只占前排;
// 怪物按副本怪物组顺序落位,组内超过 5 只落后排
inline constexpr uint32_t kFormationFrontRowSize = 5;

// ---- PVE 怪物只数随队伍人数(docs/design/pve-team-size-matching.md §2) ----

// 按人数放大后的怪物只数上限 = 阵位前后两排(D4)。超出的部分不生成;
// 副本怪物组原样生成(每人只数为 0)的路径不受此限,那里的只数由表的槽位数决定。
// 「每人几只」不是常量:由 BattleDataProvider::GetDungeonMonstersPerPlayer 按副本给
inline constexpr uint32_t kMaxScaledPveMonsterCount = kFormationFrontRowSize * 2;

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
// player_id 只由发号器发(data_service.AllocateIdSegment,AGENTS.md §7 第 1 条),是计数器值经 MixID
// 打散后的 63 位值,恒 < 2^63(go/shared/idsegment/mix.go);且 InitPlayers 对 bit63 置位的 player_id
// 直接 fail-closed —— 这是被强制的不变量,不是假设。
inline constexpr uint64_t kEngineLocalActorIdFlag = 1ULL << 63;
inline constexpr uint64_t kMonsterActorIdBase = kEngineLocalActorIdFlag | (1ULL << 32);  // 怪物局内 actor_id 起始值
inline constexpr uint64_t kPetActorIdBase = kEngineLocalActorIdFlag | (2ULL << 32);      // 宝宝局内 actor_id 起始值
inline constexpr uint64_t kMonsterDefaultHealth = 300;
inline constexpr uint64_t kMonsterDefaultStrength = 5;
inline constexpr uint64_t kMonsterDefaultArmor = 24;  // 2026-09-14 防御单位 ×12(原 2)
inline constexpr uint64_t kMonsterDefaultResistance = 0;
inline constexpr uint64_t kMonsterDefaultCritChance = 0;
inline constexpr uint64_t kMonsterDefaultSpeed = 60;  // 2026-09-14 速度单位 ×12(原 5)

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
