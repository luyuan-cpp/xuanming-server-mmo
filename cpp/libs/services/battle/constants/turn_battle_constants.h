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
// player_id 两个来源都 < 2^63(号段从 1 递增;遗留 bwmarrin SnowFlake 是正的 int64),
// 且 InitPlayers 对 bit63 置位的 player_id 直接 fail-closed —— 这是被强制的不变量,不是假设。
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
