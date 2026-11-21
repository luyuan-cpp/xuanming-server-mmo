package combatai

import "sync"

// 副本怪物智能决策模块（纯 Go 标准库，零 protobuf 耦合，与 C++ turnbattle::DungeonMonsterAiController 语义与权重逐位对齐）。
//
// 架构分工：
//   1. Boss 宏观阶段（HFSM）：Root -> Combat -> { Opening / Enrage / Desperate } 与 Dead，LCA 切换不重复进出 Combat。
//   2. 战术行为树（BehaviorTree）：进程级共享只读拓扑，单怪持有轻量 BehaviorTreeState。
//   3. 数据训练效用打分（LinearUtilityModel）：与 C++ / Java 使用相同 6 维特征与 8 条基准偏好样本，产出逐位一致的 int32 权重。

const MonsterFeatureCount = 6

const (
	FeatureTargetMissingHP    = 0 // 目标已损失气血万分比 (0..10000)
	FeatureTargetUncontrolled = 1 // 目标未被控制 (10000 / 0)
	FeatureTargetThreat       = 2 // 目标速度/压制威胁度万分比 (0..10000)
	FeatureIsSkill            = 3 // 技能施放 (10000) vs 普攻 (0)
	FeaturePhaseUrgency       = 4 // Boss 阶段紧迫度：Opening=0, Enrage=5000, Desperate=10000
	FeatureTargetIsPlayer     = 5 // 目标是玩家 (10000) vs 宠物 (3000)
)

// 行动类型常量，与 proto eBattleActionType 枚举值一致。
const (
	MonsterActionAttack uint32 = 1
	MonsterActionSkill  uint32 = 2
	MonsterActionDefend uint32 = 3
)

type BossPhaseID HfsmStateID

const (
	BossPhaseRoot      BossPhaseID = 0
	BossPhaseCombat    BossPhaseID = 1
	BossPhaseOpening   BossPhaseID = 2
	BossPhaseEnrage    BossPhaseID = 3
	BossPhaseDesperate BossPhaseID = 4
	BossPhaseDead      BossPhaseID = 5
)

type MonsterBattleAction struct {
	ActionType   uint32
	SkillTableID uint32
	TargetID     uint64
}

type DungeonMonsterAiProfile struct {
	EnrageHPPermyriad          uint32
	DesperateHPPermyriad       uint32
	EmergencyDefendHPPermyriad uint32
	DefendCooldownRounds       uint32
	MonsterSkillIDs            []uint32
	UtilityModel               LinearUtilityModel
}

func DefaultDungeonMonsterAiProfile() DungeonMonsterAiProfile {
	return DungeonMonsterAiProfile{
		EnrageHPPermyriad:          6000,
		DesperateHPPermyriad:       3000,
		EmergencyDefendHPPermyriad: 2500,
		DefendCooldownRounds:       3,
		UtilityModel:               LinearUtilityModel{Weights: make([]int32, MonsterFeatureCount)},
	}
}

type MonsterActionCandidate struct {
	Action        MonsterBattleAction
	FormationSlot uint32
	Features      [MonsterFeatureCount]int32
}

type MonsterDecisionInput struct {
	RoundIndex      uint32
	SelfHPPermyriad uint32
	SelfAlive       bool
	SelfCanDefend   bool
	Candidates      []MonsterActionCandidate
}

// TrainMonsterAiProfile 用对战偏好样本训练副本怪物效用权重（确定性全批量逻辑回归）。
func TrainMonsterAiProfile(
	samples []PairwiseTrainingSample,
	baseProfile DungeonMonsterAiProfile,
) (DungeonMonsterAiProfile, PairwiseTrainMetrics) {
	result := baseProfile
	model, metrics := TrainPairwiseLinearModel(MonsterFeatureCount, samples, DefaultPairwiseTrainConfig())
	result.UtilityModel = model
	return result, metrics
}

// BuildTrainedDefaultMonsterAiProfile 内置默认副本 Boss 配置：与 C++ / Java 使用完全相同的 8 条战术偏好样本。
func BuildTrainedDefaultMonsterAiProfile() DungeonMonsterAiProfile {
	samples := []PairwiseTrainingSample{
		{Chosen: []int32{8000, 10000, 8000, 10000, 5000, 10000}, Rejected: []int32{1000, 0, 2000, 0, 0, 3000}, SampleWeight: 1.0},
		{Chosen: []int32{7500, 10000, 6000, 10000, 5000, 10000}, Rejected: []int32{7500, 10000, 6000, 0, 5000, 10000}, SampleWeight: 1.0},
		{Chosen: []int32{9000, 10000, 5000, 0, 0, 10000}, Rejected: []int32{1500, 10000, 5000, 0, 0, 10000}, SampleWeight: 1.0},
		{Chosen: []int32{4000, 10000, 9000, 10000, 5000, 10000}, Rejected: []int32{4000, 0, 3000, 10000, 5000, 10000}, SampleWeight: 1.0},
		{Chosen: []int32{5000, 10000, 7000, 0, 0, 10000}, Rejected: []int32{5000, 10000, 7000, 0, 0, 3000}, SampleWeight: 1.0},
		{Chosen: []int32{6500, 10000, 7500, 10000, 10000, 10000}, Rejected: []int32{2000, 0, 4000, 0, 10000, 10000}, SampleWeight: 1.0},
		{Chosen: []int32{3000, 10000, 8500, 10000, 5000, 10000}, Rejected: []int32{3000, 10000, 2000, 10000, 5000, 10000}, SampleWeight: 1.0},
		{Chosen: []int32{8500, 0, 6000, 10000, 10000, 10000}, Rejected: []int32{1000, 10000, 2000, 0, 0, 3000}, SampleWeight: 1.0},
	}
	profile, _ := TrainMonsterAiProfile(samples, DefaultDungeonMonsterAiProfile())
	return profile
}

const bbDefendCooldownReadyRound uint32 = 1

type monsterBtCtx struct {
	input          *MonsterDecisionInput
	profile        *DungeonMonsterAiProfile
	currentPhase   BossPhaseID
	selectedAction MonsterBattleAction
	hasSelected    bool
}

var (
	sharedDungeonTreeOnce sync.Once
	sharedDungeonTree     *BehaviorTreeDef[*monsterBtCtx]
)

func getSharedDungeonMonsterTree() *BehaviorTreeDef[*monsterBtCtx] {
	sharedDungeonTreeOnce.Do(func() {
		b := NewBehaviorTreeBuilder[*monsterBtCtx]()

		desperateSurvival := b.Sequence(
			b.Condition(func(ctx *monsterBtCtx, bb *Blackboard) bool {
				in := ctx.input
				prof := ctx.profile
				if !in.SelfCanDefend || ctx.currentPhase != BossPhaseDesperate {
					return false
				}
				if in.SelfHPPermyriad > prof.EmergencyDefendHPPermyriad {
					return false
				}
				readyRound := bb.GetUint64Or(bbDefendCooldownReadyRound, 0)
				return uint64(in.RoundIndex) >= readyRound
			}),
			b.Action(func(ctx *monsterBtCtx, bb *Blackboard) BtStatus {
				ctx.selectedAction = MonsterBattleAction{ActionType: MonsterActionDefend, TargetID: 0}
				ctx.hasSelected = true
				bb.SetUint64(bbDefendCooldownReadyRound, uint64(ctx.input.RoundIndex)+uint64(ctx.profile.DefendCooldownRounds))
				return BtSuccess
			}, nil),
		)

		utilityDecision := b.Action(func(ctx *monsterBtCtx, _ *Blackboard) BtStatus {
			in := ctx.input
			if len(in.Candidates) == 0 {
				return BtFailure
			}
			phaseUrgency := int32(0)
			if ctx.currentPhase == BossPhaseDesperate {
				phaseUrgency = UtilityScale
			} else if ctx.currentPhase == BossPhaseEnrage {
				phaseUrgency = UtilityScale / 2
			}

			scored := make([]ActionCandidate, len(in.Candidates))
			for i, cand := range in.Candidates {
				actionID := cand.Action.ActionType
				if cand.Action.ActionType == MonsterActionSkill {
					actionID = cand.Action.SkillTableID
				}
				feats := cand.Features
				feats[FeaturePhaseUrgency] = phaseUrgency
				scored[i] = ActionCandidate{
					ActionID:      actionID,
					TargetID:      cand.Action.TargetID,
					FormationSlot: cand.FormationSlot,
					Features:      feats[:],
				}
			}

			best, ok := SelectBestCandidate(scored, ctx.profile.UtilityModel)
			if !ok {
				return BtFailure
			}
			ctx.selectedAction = in.Candidates[best.Index].Action
			ctx.hasSelected = true
			return BtSuccess
		}, nil)

		root := b.ReactiveSelector(desperateSurvival, utilityDecision)
		tree, ok := b.Build(root)
		if !ok {
			panic("shared dungeon monster behavior tree must be acyclic")
		}
		sharedDungeonTree = tree
	})
	return sharedDungeonTree
}

type monsterPhaseEvent uint32

const monsterPhaseEventDie monsterPhaseEvent = 1

type monsterPhaseCtx struct {
	selfHPPermyriad      uint32
	enrageHPPermyriad    uint32
	desperateHPPermyriad uint32
	combatEnterCount     uint32
	combatExitCount      uint32
}

// DungeonMonsterAiController 单只副本怪的运行期 AI 控制器。
type DungeonMonsterAiController struct {
	profile   DungeonMonsterAiProfile
	phaseCtx  monsterPhaseCtx
	phaseHfsm *Hfsm[*monsterPhaseCtx, monsterPhaseEvent]
	btState   BehaviorTreeState
}

func NewDungeonMonsterAiController(profile DungeonMonsterAiProfile) *DungeonMonsterAiController {
	c := &DungeonMonsterAiController{
		profile: profile,
		phaseCtx: monsterPhaseCtx{
			selfHPPermyriad:      uint32(UtilityScale),
			enrageHPPermyriad:    profile.EnrageHPPermyriad,
			desperateHPPermyriad: profile.DesperateHPPermyriad,
		},
		phaseHfsm: NewHfsm[*monsterPhaseCtx, monsterPhaseEvent](),
		btState:   getSharedDungeonMonsterTree().CreateState(),
	}
	c.initPhaseHfsm()
	return c
}

func (c *DungeonMonsterAiController) initPhaseHfsm() {
	rootCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	rootCfg.InitialChild = HfsmStateID(BossPhaseCombat)
	rootCfg.OnEvent = func(ev monsterPhaseEvent, _ *monsterPhaseCtx) HfsmEventOutcome {
		if ev == monsterPhaseEventDie {
			return HfsmTransitionTo(HfsmStateID(BossPhaseDead))
		}
		return HfsmIgnored()
	}
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseRoot), rootCfg)

	combatCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	combatCfg.Parent = HfsmStateID(BossPhaseRoot)
	combatCfg.InitialChild = HfsmStateID(BossPhaseOpening)
	combatCfg.OnEnter = func(ctx *monsterPhaseCtx) { ctx.combatEnterCount++ }
	combatCfg.OnExit = func(ctx *monsterPhaseCtx) { ctx.combatExitCount++ }
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseCombat), combatCfg)

	openingCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	openingCfg.Parent = HfsmStateID(BossPhaseCombat)
	openingCfg.OnUpdate = func(ctx *monsterPhaseCtx, _ uint32) (HfsmStateID, bool) {
		if ctx.selfHPPermyriad <= ctx.desperateHPPermyriad {
			return HfsmStateID(BossPhaseDesperate), true
		}
		if ctx.selfHPPermyriad <= ctx.enrageHPPermyriad {
			return HfsmStateID(BossPhaseEnrage), true
		}
		return InvalidHfsmState, false
	}
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseOpening), openingCfg)

	enrageCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	enrageCfg.Parent = HfsmStateID(BossPhaseCombat)
	enrageCfg.OnUpdate = func(ctx *monsterPhaseCtx, _ uint32) (HfsmStateID, bool) {
		if ctx.selfHPPermyriad <= ctx.desperateHPPermyriad {
			return HfsmStateID(BossPhaseDesperate), true
		}
		return InvalidHfsmState, false
	}
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseEnrage), enrageCfg)

	desperateCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	desperateCfg.Parent = HfsmStateID(BossPhaseCombat)
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseDesperate), desperateCfg)

	deadCfg := NewHfsmStateConfig[*monsterPhaseCtx, monsterPhaseEvent]()
	deadCfg.Parent = HfsmStateID(BossPhaseRoot)
	c.phaseHfsm.AddState(HfsmStateID(BossPhaseDead), deadCfg)

	c.phaseHfsm.Start(HfsmStateID(BossPhaseRoot), &c.phaseCtx)
}

func (c *DungeonMonsterAiController) DecideAction(input *MonsterDecisionInput) (MonsterBattleAction, bool) {
	if input == nil || !input.SelfAlive {
		c.NotifyDead()
		return MonsterBattleAction{}, false
	}
	c.phaseCtx.selfHPPermyriad = input.SelfHPPermyriad
	c.phaseHfsm.Update(&c.phaseCtx, 1)

	btCtx := monsterBtCtx{
		input:        input,
		profile:      &c.profile,
		currentPhase: c.CurrentPhase(),
	}
	if getSharedDungeonMonsterTree().Tick(&btCtx, &c.btState) != BtSuccess || !btCtx.hasSelected {
		return MonsterBattleAction{}, false
	}
	return btCtx.selectedAction, true
}

func (c *DungeonMonsterAiController) NotifyDead() {
	if c.phaseHfsm.Started() && c.CurrentPhase() != BossPhaseDead {
		c.phaseHfsm.Dispatch(monsterPhaseEventDie, &c.phaseCtx)
	}
}

func (c *DungeonMonsterAiController) CurrentPhase() BossPhaseID {
	return BossPhaseID(c.phaseHfsm.CurrentLeaf())
}

func (c *DungeonMonsterAiController) IsInCombat() bool {
	return c.phaseHfsm.IsInState(HfsmStateID(BossPhaseCombat))
}

func (c *DungeonMonsterAiController) CombatEnterCount() uint32 {
	return c.phaseCtx.combatEnterCount
}

func (c *DungeonMonsterAiController) CombatExitCount() uint32 {
	return c.phaseCtx.combatExitCount
}

func (c *DungeonMonsterAiController) Profile() DungeonMonsterAiProfile {
	return c.profile
}
