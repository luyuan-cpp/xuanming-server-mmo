#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <span>
#include <vector>

#include "modules/combat_ai/behavior_tree.h"
#include "modules/combat_ai/hfsm.h"
#include "modules/combat_ai/monster_utility_ai.h"
#include "proto/battle/battle_data.pb.h"

// 副本怪物智能决策模块(纯确定性,零随机数,零挂钟,无裸指针成员)。
//
// 架构分工:
// 1. Boss 宏观阶段(HFSM): Root -> Combat -> { Opening / Enrage / Desperate } 与 Dead;
//    阶段切换走 LCA,共享父态 Combat 不重复退出重入。
// 2. 战术行为树(BehaviorTree): 全节点共享一份不可变拓扑 BehaviorTreeDef,
//    每只怪仅持轻量 BehaviorTreeState(冷却按回合 tick 计数,确定性)。
//    - 高优分支(ReactiveSelector): 濒死阶段触发带冷却的防御自保;
//    - 常规分支: 枚举已通过 CheckActionPrerequisites 的全部合法 (行动, 目标) 候选,
//      交由数据训练出的定点整数效用模型 LinearUtilityModel 打分并稳定选优。
// 3. 离线/单测训练入口: BuildTrainedDefaultProfile / TrainMonsterAiProfile 使用
//    combat_ai::TrainPairwiseLinearModel 从对战偏好数据训练出 int32_t 权重表,
//    线上零外部 ML 依赖且保证 C++ / 未来 Java 逐字节确定性回放。

namespace turnbattle
{

inline constexpr std::size_t kMonsterFeatureCount = 6;

enum MonsterFeatureIndex : std::size_t
{
    kFeatureTargetMissingHp = 0,    // 目标已损失气血万分比(0..10000),鼓励集火残血
    kFeatureTargetUncontrolled = 1, // 目标尚未被控(眩晕/沉默/冰冻)为 10000,否则 0
    kFeatureTargetThreat = 2,       // 目标速度/压制威胁度万分比(0..10000)
    kFeatureIsSkill = 3,            // 本候选为技能施放为 10000,普攻为 0
    kFeaturePhaseUrgency = 4,       // Boss 阶段紧迫度: Opening=0, Enrage=5000, Desperate=10000
    kFeatureTargetIsPlayer = 5,     // 目标是玩家(10000) vs 宠物(3000)
};

enum class BossPhaseId : combat_ai::HfsmStateId
{
    kRoot = 0,
    kCombat = 1,
    kPhaseOpening = 2,
    kPhaseEnrage = 3,
    kPhaseDesperate = 4,
    kDead = 5,
};

struct DungeonMonsterAiProfile
{
    uint32_t enrageHpPermyriad{6000};          // HP <= 60% 进入狂暴阶段
    uint32_t desperateHpPermyriad{3000};       // HP <= 30% 进入绝境阶段
    uint32_t emergencyDefendHpPermyriad{2500}; // 绝境且 HP <= 25% 允许触发带冷却的防御自保
    uint32_t defendCooldownRounds{3};          // 防御自保冷却回合数(tick 计数)
    // 可选为该怪物挂载的零耗蓝回合制技能 id(初始化时经 IsTurnBattleCastableSkill + LegacySkillManaCost 校验)。
    std::vector<uint32_t> monsterSkillIds{};
    combat_ai::LinearUtilityModel<kMonsterFeatureCount> utilityModel{};
};

struct MonsterActionCandidate
{
    BattleAction action{};
    uint32_t formationSlot{0};
    combat_ai::FeatureVec<kMonsterFeatureCount> features{};
};

struct MonsterDecisionInput
{
    uint32_t roundIndex{1};
    uint32_t selfHpPermyriad{10000};
    bool selfAlive{true};
    bool selfCanDefend{true};
    std::vector<MonsterActionCandidate> candidates{};
};

// 用离线/单测对战样本训练副本怪物效用权重(纯确定性批量逻辑回归,零随机数)。
DungeonMonsterAiProfile TrainMonsterAiProfile(
    std::span<const combat_ai::PairwiseTrainingSample<kMonsterFeatureCount>> samples,
    const DungeonMonsterAiProfile& baseProfile = {},
    combat_ai::PairwiseTrainMetrics* outMetrics = nullptr);

// 内置默认副本 Boss 配置:由确定性训练器基于标准副本战术样本产出量化整数权重。
DungeonMonsterAiProfile BuildTrainedDefaultMonsterAiProfile();

// 单只副本怪的运行期 AI 控制器(存放在 TurnBattleEngine::monsterAiControllers 有序 map 中)。
class DungeonMonsterAiController
{
public:
    explicit DungeonMonsterAiController(DungeonMonsterAiProfile profile);

    // 推进本回合 HFSM 阶段并执行共享行为树,返回选定的合法行动(无候选返回 std::nullopt)。
    std::optional<BattleAction> DecideAction(const MonsterDecisionInput& input);

    // 单位阵亡时触发 HFSM 死亡事件(事件冒泡至 Root -> Dead,退出 Combat 链)。
    void NotifyDead();

    [[nodiscard]] BossPhaseId CurrentPhase() const noexcept;
    [[nodiscard]] bool IsInCombat() const noexcept;
    [[nodiscard]] uint32_t CombatEnterCount() const noexcept { return phaseCtx.combatEnterCount; }
    [[nodiscard]] uint32_t CombatExitCount() const noexcept { return phaseCtx.combatExitCount; }
    [[nodiscard]] const DungeonMonsterAiProfile& Profile() const noexcept { return profile; }

private:
    enum class PhaseEvent : uint32_t
    {
        kDie = 1,
    };

    struct PhaseCtx
    {
        uint32_t selfHpPermyriad{10000};
        uint32_t enrageHpPermyriad{6000};
        uint32_t desperateHpPermyriad{3000};
        uint32_t combatEnterCount{0};
        uint32_t combatExitCount{0};
    };

    void InitPhaseHfsm();

    DungeonMonsterAiProfile profile{};
    PhaseCtx phaseCtx{};
    combat_ai::Hfsm<PhaseCtx, PhaseEvent> phaseHfsm{};
    combat_ai::BehaviorTreeState btState{};
};

} // namespace turnbattle
