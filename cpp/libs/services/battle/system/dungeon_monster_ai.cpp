#include "system/dungeon_monster_ai.h"

#include <array>
#include <utility>

namespace turnbattle
{
namespace
{

constexpr uint32_t kBbDefendCooldownReadyRound = 1;

// 行为树单次 Tick 上下文(使用 reference_wrapper 借用不可空输入,无裸指针成员)。
struct MonsterBtCtx
{
    std::reference_wrapper<const MonsterDecisionInput> input;
    std::reference_wrapper<const DungeonMonsterAiProfile> profile;
    BossPhaseId currentPhase{BossPhaseId::kPhaseOpening};
    std::optional<BattleAction> selectedAction{};
};

constexpr combat_ai::HfsmStateId ToId(BossPhaseId id) noexcept
{
    return static_cast<combat_ai::HfsmStateId>(id);
}

// 全进程共享的一份不可变副本怪物行为树定义:
//   ReactiveSelector
//     1) 绝境防御分支: Desperate 阶段 && HP <= emergencyDefendHpPermyriad && 冷却就绪 -> DEFEND
//     2) 效用选招分支: 用训练好的定点整数模型从全部合法候选中确定性挑选最高分行动
const combat_ai::BehaviorTreeDef<MonsterBtCtx>& SharedDungeonMonsterTree()
{
    static const combat_ai::BehaviorTreeDef<MonsterBtCtx> kTree = [] {
        combat_ai::BehaviorTreeBuilder<MonsterBtCtx> b;

        const auto desperateSurvival = b.Sequence({
            b.Condition([](const MonsterBtCtx& ctx, const combat_ai::Blackboard& bb) {
                const auto& in = ctx.input.get();
                const auto& prof = ctx.profile.get();
                if (!in.selfCanDefend || ctx.currentPhase != BossPhaseId::kPhaseDesperate)
                {
                    return false;
                }
                if (in.selfHpPermyriad > prof.emergencyDefendHpPermyriad)
                {
                    return false;
                }
                const uint64_t readyRound = bb.GetOr<uint64_t>(kBbDefendCooldownReadyRound, 0);
                return static_cast<uint64_t>(in.roundIndex) >= readyRound;
            }),
            b.Action([](MonsterBtCtx& ctx, combat_ai::Blackboard& bb) {
                const auto& in = ctx.input.get();
                const auto& prof = ctx.profile.get();
                BattleAction defend;
                defend.set_action_type(BATTLE_ACTION_DEFEND);
                defend.set_target_id(0);
                ctx.selectedAction = std::move(defend);
                bb.Set<uint64_t>(kBbDefendCooldownReadyRound,
                                 static_cast<uint64_t>(in.roundIndex) + prof.defendCooldownRounds);
                return combat_ai::BtStatus::kSuccess;
            }),
        });

        const auto utilityDecision = b.Action([](MonsterBtCtx& ctx, combat_ai::Blackboard&) {
            const auto& in = ctx.input.get();
            if (in.candidates.empty())
            {
                return combat_ai::BtStatus::kFailure;
            }
            std::vector<combat_ai::ActionCandidate<kMonsterFeatureCount>> scored;
            scored.reserve(in.candidates.size());
            const int32_t phaseUrgency =
                (ctx.currentPhase == BossPhaseId::kPhaseDesperate)
                    ? combat_ai::kUtilityScale
                    : (ctx.currentPhase == BossPhaseId::kPhaseEnrage ? (combat_ai::kUtilityScale / 2) : 0);

            for (const auto& cand : in.candidates)
            {
                combat_ai::ActionCandidate<kMonsterFeatureCount> item;
                item.actionId = cand.action.action_type() == BATTLE_ACTION_SKILL
                                    ? cand.action.skill_table_id()
                                    : static_cast<uint32_t>(cand.action.action_type());
                item.targetId = cand.action.target_id();
                item.formationSlot = cand.formationSlot;
                item.features = cand.features;
                item.features[kFeaturePhaseUrgency] = phaseUrgency;
                scored.push_back(item);
            }

            const auto best = combat_ai::SelectBestCandidate<kMonsterFeatureCount>(
                scored, ctx.profile.get().utilityModel);
            if (!best.has_value())
            {
                return combat_ai::BtStatus::kFailure;
            }
            ctx.selectedAction = in.candidates[best->index].action;
            return combat_ai::BtStatus::kSuccess;
        });

        const auto root = b.ReactiveSelector({desperateSurvival, utilityDecision});
        auto built = b.Build(root);
        return std::move(*built);
    }();
    return kTree;
}

} // namespace

DungeonMonsterAiProfile TrainMonsterAiProfile(
    std::span<const combat_ai::PairwiseTrainingSample<kMonsterFeatureCount>> samples,
    const DungeonMonsterAiProfile& baseProfile,
    combat_ai::PairwiseTrainMetrics* outMetrics)
{
    DungeonMonsterAiProfile result = baseProfile;
    result.utilityModel = combat_ai::TrainPairwiseLinearModel<kMonsterFeatureCount>(samples, {}, outMetrics);
    return result;
}

DungeonMonsterAiProfile BuildTrainedDefaultMonsterAiProfile()
{
    // 标准副本战术偏好样本(顺序: 残血度 / 未被控 / 威胁度 / 技能优先 / 阶段紧迫 / 目标是玩家):
    // 1) 优先用技能集火残血且未受控的高威胁玩家;
    // 2) 同条件下优先攻击玩家而非宠物;
    // 3) 狂暴/绝境阶段更倾向释放技能抢伤害。
    static const std::array<combat_ai::PairwiseTrainingSample<kMonsterFeatureCount>, 8> kDefaultSamples{{
        {{8000, 10000, 8000, 10000, 5000, 10000}, {1000, 0, 2000, 0, 0, 3000}, 1.0},
        {{7500, 10000, 6000, 10000, 5000, 10000}, {7500, 10000, 6000, 0, 5000, 10000}, 1.0},
        {{9000, 10000, 5000, 0, 0, 10000}, {1500, 10000, 5000, 0, 0, 10000}, 1.0},
        {{4000, 10000, 9000, 10000, 5000, 10000}, {4000, 0, 3000, 10000, 5000, 10000}, 1.0},
        {{5000, 10000, 7000, 0, 0, 10000}, {5000, 10000, 7000, 0, 0, 3000}, 1.0},
        {{6500, 10000, 7500, 10000, 10000, 10000}, {2000, 0, 4000, 0, 10000, 10000}, 1.0},
        {{3000, 10000, 8500, 10000, 5000, 10000}, {3000, 10000, 2000, 10000, 5000, 10000}, 1.0},
        {{8500, 0, 6000, 10000, 10000, 10000}, {1000, 10000, 2000, 0, 0, 3000}, 1.0},
    }};
    return TrainMonsterAiProfile(kDefaultSamples);
}

DungeonMonsterAiController::DungeonMonsterAiController(DungeonMonsterAiProfile profile)
    : profile_(std::move(profile))
    , btState_(SharedDungeonMonsterTree().CreateState())
{
    phaseCtx_.enrageHpPermyriad = profile_.enrageHpPermyriad;
    phaseCtx_.desperateHpPermyriad = profile_.desperateHpPermyriad;
    InitPhaseHfsm();
}

void DungeonMonsterAiController::InitPhaseHfsm()
{
    phaseHfsm_.AddState(
        ToId(BossPhaseId::kRoot),
        {
            .parent = combat_ai::kInvalidHfsmState,
            .initialChild = ToId(BossPhaseId::kCombat),
            .onEvent = [](const PhaseEvent& ev, PhaseCtx&) {
                return ev == PhaseEvent::kDie
                           ? combat_ai::HfsmEventOutcome::TransitionTo(ToId(BossPhaseId::kDead))
                           : combat_ai::HfsmEventOutcome::Ignored();
            },
        });

    phaseHfsm_.AddState(
        ToId(BossPhaseId::kCombat),
        {
            .parent = ToId(BossPhaseId::kRoot),
            .initialChild = ToId(BossPhaseId::kPhaseOpening),
            .onEnter = [](PhaseCtx& ctx) { ++ctx.combatEnterCount; },
            .onExit = [](PhaseCtx& ctx) { ++ctx.combatExitCount; },
        });

    phaseHfsm_.AddState(
        ToId(BossPhaseId::kPhaseOpening),
        {
            .parent = ToId(BossPhaseId::kCombat),
            .onUpdate = [](PhaseCtx& ctx, uint32_t) -> std::optional<combat_ai::HfsmStateId> {
                if (ctx.selfHpPermyriad <= ctx.desperateHpPermyriad)
                {
                    return ToId(BossPhaseId::kPhaseDesperate);
                }
                if (ctx.selfHpPermyriad <= ctx.enrageHpPermyriad)
                {
                    return ToId(BossPhaseId::kPhaseEnrage);
                }
                return std::nullopt;
            },
        });

    phaseHfsm_.AddState(
        ToId(BossPhaseId::kPhaseEnrage),
        {
            .parent = ToId(BossPhaseId::kCombat),
            .onUpdate = [](PhaseCtx& ctx, uint32_t) -> std::optional<combat_ai::HfsmStateId> {
                if (ctx.selfHpPermyriad <= ctx.desperateHpPermyriad)
                {
                    return ToId(BossPhaseId::kPhaseDesperate);
                }
                return std::nullopt;
            },
        });

    phaseHfsm_.AddState(
        ToId(BossPhaseId::kPhaseDesperate),
        {
            .parent = ToId(BossPhaseId::kCombat),
        });

    phaseHfsm_.AddState(
        ToId(BossPhaseId::kDead),
        {
            .parent = ToId(BossPhaseId::kRoot),
        });

    phaseHfsm_.Start(ToId(BossPhaseId::kRoot), phaseCtx_);
}

std::optional<BattleAction> DungeonMonsterAiController::DecideAction(const MonsterDecisionInput& input)
{
    if (!input.selfAlive)
    {
        NotifyDead();
        return std::nullopt;
    }

    phaseCtx_.selfHpPermyriad = input.selfHpPermyriad;
    phaseHfsm_.Update(phaseCtx_, 1);

    MonsterBtCtx btCtx{std::cref(input), std::cref(profile_), CurrentPhase(), std::nullopt};
    const combat_ai::BtStatus status = SharedDungeonMonsterTree().Tick(btCtx, btState_);
    if (status != combat_ai::BtStatus::kSuccess)
    {
        return std::nullopt;
    }
    return btCtx.selectedAction;
}

void DungeonMonsterAiController::NotifyDead()
{
    if (phaseHfsm_.Started() && CurrentPhase() != BossPhaseId::kDead)
    {
        phaseHfsm_.Dispatch(PhaseEvent::kDie, phaseCtx_);
    }
}

BossPhaseId DungeonMonsterAiController::CurrentPhase() const noexcept
{
    return static_cast<BossPhaseId>(phaseHfsm_.CurrentLeaf());
}

bool DungeonMonsterAiController::IsInCombat() const noexcept
{
    return phaseHfsm_.IsInState(ToId(BossPhaseId::kCombat));
}

} // namespace turnbattle
