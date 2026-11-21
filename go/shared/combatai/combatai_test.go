package combatai

import (
	"slices"
	"strings"
	"testing"
)

func TestStateMutexMatrix(t *testing.T) {
	const (
		stateCombat     uint32 = 0
		stateTeamFollow uint32 = 1
		stateMounted    uint32 = 2
		stateSilence    uint32 = 3
		stateCount      uint32 = 4

		actionUseSkill   uint32 = 0
		actionJoinFollow uint32 = 1
		actionMount      uint32 = 2

		tipSuccess       uint32 = 1000
		tipSilenced      uint32 = 1001
		tipInCombat      uint32 = 1002
		tipUnknownAction uint32 = 9001
		tipInvalidState  uint32 = 9002
	)

	matrix := NewStateMutexMatrix()
	invalid := matrix.LoadRules([]StateRule{
		{Action: actionUseSkill, State: stateCombat, Mode: StateModeAllow, Tip: 0},
		{Action: actionUseSkill, State: stateTeamFollow, Mode: StateModeInterrupt, Tip: 0},
		{Action: actionUseSkill, State: stateMounted, Mode: StateModeInterrupt, Tip: 0},
		{Action: actionUseSkill, State: stateSilence, Mode: StateModeBlock, Tip: tipSilenced},
		{Action: actionMount, State: stateCombat, Mode: StateModeBlock, Tip: tipInCombat},
	})
	if invalid != 0 {
		t.Fatalf("expected 0 invalid rules, got %d", invalid)
	}

	var states RefCountedStateBitset
	if !states.Acquire(stateTeamFollow) || !states.Acquire(stateMounted) || !states.Acquire(stateSilence) {
		t.Fatalf("expected initial state acquires to flip active bits")
	}
	if states.Acquire(stateSilence) {
		t.Fatalf("expected second silence acquire not to report newly active")
	}
	if got := states.RefCount(stateSilence); got != 2 {
		t.Fatalf("expected silence refcount 2, got %d", got)
	}

	var exited []uint32
	var hooks StateExitHookRegistry[uint64]
	hooks.Register(stateTeamFollow, func(_ uint64, reason StateExitReason) {
		if reason != StateExitInterrupted {
			t.Errorf("expected interrupted reason, got %v", reason)
		}
		if states.Has(stateTeamFollow) {
			t.Errorf("expected state cleared before exit hook runs")
		}
		exited = append(exited, stateTeamFollow)
	})
	hooks.Register(stateMounted, func(_ uint64, reason StateExitReason) {
		if reason != StateExitInterrupted {
			t.Errorf("expected interrupted reason, got %v", reason)
		}
		if states.Has(stateMounted) {
			t.Errorf("expected state cleared before exit hook runs")
		}
		exited = append(exited, stateMounted)
	})

	// 沉默拦截时两阶段门禁零副作用：不得提前打断跟随与骑乘。
	blockedTip := matrix.TryPerform(
		states.Mask(), actionUseSkill, stateCombat, stateCount,
		tipSuccess, tipUnknownAction, tipInvalidState,
		func(s uint32) {
			if states.ClearState(s) {
				hooks.NotifyExited(100, s, StateExitInterrupted)
			}
		},
		func(s uint32) { states.Acquire(s) },
	)
	if blockedTip != tipSilenced {
		t.Fatalf("expected tip %d, got %d", tipSilenced, blockedTip)
	}
	if !states.Has(stateTeamFollow) || !states.Has(stateMounted) || states.Has(stateCombat) || len(exited) != 0 {
		t.Fatalf("expected zero side effects on blocked action")
	}

	// 释放双层沉默后再施法：自动打断 TeamFollow & Mounted 并触发钩子，再入 Combat。
	if states.Release(stateSilence) || !states.Has(stateSilence) {
		t.Fatalf("expected silence still active after first release")
	}
	if !states.Release(stateSilence) || states.Has(stateSilence) {
		t.Fatalf("expected silence cleared after second release")
	}

	okTip := matrix.TryPerform(
		states.Mask(), actionUseSkill, stateCombat, stateCount,
		tipSuccess, tipUnknownAction, tipInvalidState,
		func(s uint32) {
			if states.ClearState(s) {
				hooks.NotifyExited(100, s, StateExitInterrupted)
			}
		},
		func(s uint32) { states.Acquire(s) },
	)
	if okTip != tipSuccess {
		t.Fatalf("expected success tip %d, got %d", tipSuccess, okTip)
	}
	if states.Has(stateTeamFollow) || states.Has(stateMounted) || !states.Has(stateCombat) {
		t.Fatalf("unexpected states after TryPerform")
	}
	if !slices.Equal(exited, []uint32{stateTeamFollow, stateMounted}) {
		t.Fatalf("unexpected exited hooks: %v", exited)
	}

	// 战斗中上马拦截、未知动作、越界入态。
	if got := matrix.CanPerform(states.Mask(), actionMount, tipSuccess, tipUnknownAction); got != tipInCombat {
		t.Fatalf("expected %d, got %d", tipInCombat, got)
	}
	if got := matrix.CanPerform(states.Mask(), 999, tipSuccess, tipUnknownAction); got != tipUnknownAction {
		t.Fatalf("expected %d, got %d", tipUnknownAction, got)
	}
	if got := matrix.TryPerform(
		states.Mask(), actionUseSkill, stateCount, stateCount,
		tipSuccess, tipUnknownAction, tipInvalidState,
		func(uint32) { t.Fatal("should not interrupt") },
		func(uint32) { t.Fatal("should not enter") },
	); got != tipInvalidState {
		t.Fatalf("expected %d, got %d", tipInvalidState, got)
	}
}

func TestFsmFullLooplabFeatures(t *testing.T) {
	type castState uint8
	const (
		csIdle castState = iota
		csWindup
		csChannel
		csRecovery
		csDead
	)
	type castEvent uint8
	const (
		ceStartCast castEvent = iota
		ceWindupDone
		ceChannelDone
		ceRecoveryDone
		ceDie
	)
	type castCtx struct {
		hasMana             bool
		instantSkipRecovery bool
		trace               []string
	}

	fsm := NewFsm[castState, castEvent, *castCtx](csIdle)
	fsm.AddTransition(csIdle, ceStartCast, csWindup,
		func(c *castCtx) bool { return c.hasMana },
		func(_, _ castState, _ castEvent, c *castCtx) { c.trace = append(c.trace, "tx:start") })
	fsm.AddTransition(csWindup, ceWindupDone, csChannel, nil, nil)
	fsm.AddTransition(csChannel, ceChannelDone, csRecovery, nil, nil)
	fsm.AddTransition(csRecovery, ceRecoveryDone, csIdle, nil, nil)
	fsm.AddAnyTransition(ceDie, csDead, nil, nil, false)

	fsm.OnLeave(csIdle, func(c *castCtx) { c.trace = append(c.trace, "leave:idle") })
	fsm.OnEnter(csWindup, func(c *castCtx) { c.trace = append(c.trace, "enter:windup") })
	fsm.OnEnter(csRecovery, func(c *castCtx) {
		c.trace = append(c.trace, "enter:recovery")
		if c.instantSkipRecovery && !fsm.Trigger(ceRecoveryDone, c) {
			t.Error("expected re-entrant trigger to enqueue")
		}
	})
	fsm.OnEnter(csIdle, func(c *castCtx) { c.trace = append(c.trace, "enter:idle") })

	ctx := &castCtx{}
	if fsm.CanTrigger(ceStartCast, ctx) || fsm.Trigger(ceStartCast, ctx) || !fsm.Is(csIdle) || len(ctx.trace) != 0 {
		t.Fatalf("expected guard reject with zero side effects")
	}

	ctx.hasMana = true
	ctx.instantSkipRecovery = true
	if !fsm.Trigger(ceStartCast, ctx) || !fsm.Is(csWindup) {
		t.Fatalf("expected windup state")
	}
	if !slices.Equal(ctx.trace, []string{"leave:idle", "tx:start", "enter:windup"}) {
		t.Fatalf("unexpected trace: %v", ctx.trace)
	}
	if !fsm.Trigger(ceWindupDone, ctx) || !fsm.Is(csChannel) {
		t.Fatalf("expected channel state")
	}
	if !fsm.Trigger(ceChannelDone, ctx) || !fsm.Is(csIdle) {
		t.Fatalf("expected deferred recoveryDone to drain to idle, got %v", fsm.Current())
	}
	if !fsm.Trigger(ceDie, ctx) || !fsm.Is(csDead) {
		t.Fatalf("expected dead state")
	}
	if fsm.Trigger(ceDie, ctx) {
		t.Fatalf("expected allowSelf=false to reject self-transition")
	}

	// 完整 looplab/fsm 特性：AddEvents 多源 / 5 阶段回调 / Cancel / Async 两阶段提交 / AvailableTransitions / Mermaid
	type doorCtx struct {
		cancelBefore bool
		asyncLeave   bool
		steps        []string
	}
	door := NewFsm[string, string, *doorCtx]("Closed")
	door.AddEvents(
		EventDesc[string, string, *doorCtx]{Event: "open", Srcs: []string{"Closed"}, Dst: "Open"},
		EventDesc[string, string, *doorCtx]{Event: "close", Srcs: []string{"Open"}, Dst: "Closed"},
		EventDesc[string, string, *doorCtx]{Event: "lock", Srcs: []string{"Closed", "Open"}, Dst: "Locked"},
	)
	door.OnBeforeEvent("open", func(ev *FsmEventView[string, string], c *doorCtx) {
		c.steps = append(c.steps, "before:open")
		if c.cancelBefore {
			ev.Cancel(7004)
		}
	})
	door.OnBeforeAnyEvent(func(_ *FsmEventView[string, string], c *doorCtx) { c.steps = append(c.steps, "before:any") })
	door.OnLeaveState("Closed", func(ev *FsmEventView[string, string], c *doorCtx) {
		c.steps = append(c.steps, "leave:closed")
		if c.asyncLeave {
			ev.Async()
		}
	})
	door.OnEnterState("Open", func(_ *FsmEventView[string, string], c *doorCtx) { c.steps = append(c.steps, "enter:open") })
	door.OnAfterEvent("open", func(_ *FsmEventView[string, string], c *doorCtx) { c.steps = append(c.steps, "after:open") })
	door.OnAfterAnyEvent(func(_ *FsmEventView[string, string], c *doorCtx) { c.steps = append(c.steps, "after:any") })

	dctx := &doorCtx{}
	if avail := door.AvailableTransitions(dctx); !slices.Equal(avail, []string{"open", "lock"}) {
		t.Fatalf("unexpected available transitions: %v", avail)
	}

	dctx.cancelBefore = true
	res := door.TriggerEx("open", dctx)
	if res.Status != FsmStatusCanceled || res.CancelError != 7004 || !door.Is("Closed") || !slices.Equal(dctx.steps, []string{"before:open"}) {
		t.Fatalf("unexpected cancel result: %+v steps=%v", res, dctx.steps)
	}

	dctx.cancelBefore = false
	dctx.asyncLeave = true
	dctx.steps = nil
	res = door.TriggerEx("open", dctx)
	if !res.PendingAsync() || !door.HasPendingTransition() || !door.Is("Closed") {
		t.Fatalf("expected async pending transition")
	}
	if inProg := door.TriggerEx("lock", dctx); inProg.Status != FsmStatusInProgress {
		t.Fatalf("expected in-progress reject while async pending, got %v", inProg.Status)
	}
	if commit := door.CommitPendingTransition(dctx); !commit.Ok() || door.HasPendingTransition() || !door.Is("Open") {
		t.Fatalf("expected commit success")
	}
	wantSteps := []string{"before:open", "before:any", "leave:closed", "enter:open", "after:open", "after:any"}
	if !slices.Equal(dctx.steps, wantSteps) {
		t.Fatalf("unexpected steps: got %v want %v", dctx.steps, wantSteps)
	}

	mermaid := door.ExportMermaid(func(s string) string { return s }, func(e string) string { return e })
	if !strings.Contains(mermaid, "stateDiagram-v2") ||
		!strings.Contains(mermaid, "Closed --> Open : open") ||
		!strings.Contains(mermaid, "Open --> Locked : lock") {
		t.Fatalf("unexpected mermaid output:\n%s", mermaid)
	}
}

func TestHfsmLcaAndBubbling(t *testing.T) {
	const (
		sRoot HfsmStateID = iota
		sPatrol
		sCombat
		sPhase1
		sPhase2Enrage
		sDead
	)
	const (
		evAggro uint32 = 1
		evFatal uint32 = 3
	)
	type bossCtx struct {
		hpPercent        int
		combatEnterCount int
		combatExitCount  int
	}

	h := NewHfsm[*bossCtx, uint32]()

	rootCfg := NewHfsmStateConfig[*bossCtx, uint32]()
	rootCfg.InitialChild = sPatrol
	rootCfg.OnEvent = func(ev uint32, _ *bossCtx) HfsmEventOutcome {
		if ev == evFatal {
			return HfsmTransitionTo(sDead)
		}
		return HfsmIgnored()
	}
	h.AddState(sRoot, rootCfg)

	patrolCfg := NewHfsmStateConfig[*bossCtx, uint32]()
	patrolCfg.Parent = sRoot
	patrolCfg.OnEvent = func(ev uint32, _ *bossCtx) HfsmEventOutcome {
		if ev == evAggro {
			return HfsmTransitionTo(sCombat)
		}
		return HfsmIgnored()
	}
	h.AddState(sPatrol, patrolCfg)

	combatCfg := NewHfsmStateConfig[*bossCtx, uint32]()
	combatCfg.Parent = sRoot
	combatCfg.InitialChild = sPhase1
	combatCfg.OnEnter = func(c *bossCtx) { c.combatEnterCount++ }
	combatCfg.OnExit = func(c *bossCtx) { c.combatExitCount++ }
	h.AddState(sCombat, combatCfg)

	p1Cfg := NewHfsmStateConfig[*bossCtx, uint32]()
	p1Cfg.Parent = sCombat
	p1Cfg.OnUpdate = func(c *bossCtx, _ uint32) (HfsmStateID, bool) {
		if c.hpPercent <= 50 {
			return sPhase2Enrage, true
		}
		return InvalidHfsmState, false
	}
	h.AddState(sPhase1, p1Cfg)

	p2Cfg := NewHfsmStateConfig[*bossCtx, uint32]()
	p2Cfg.Parent = sCombat
	h.AddState(sPhase2Enrage, p2Cfg)

	deadCfg := NewHfsmStateConfig[*bossCtx, uint32]()
	deadCfg.Parent = sRoot
	h.AddState(sDead, deadCfg)

	ctx := &bossCtx{hpPercent: 100}
	if !h.Start(sRoot, ctx) || h.CurrentLeaf() != sPatrol || !h.IsInState(sRoot) || !h.IsInState(sPatrol) || h.IsInState(sCombat) {
		t.Fatalf("unexpected start state")
	}
	if !h.Dispatch(evAggro, ctx) || h.CurrentLeaf() != sPhase1 || !h.IsInState(sCombat) || ctx.combatEnterCount != 1 {
		t.Fatalf("expected auto-descent to phase1")
	}
	ctx.hpPercent = 40
	h.Update(ctx, 50)
	if h.CurrentLeaf() != sPhase2Enrage || !h.IsInState(sCombat) || ctx.combatEnterCount != 1 || ctx.combatExitCount != 0 {
		t.Fatalf("expected LCA transition keeping Combat active")
	}
	if !h.Dispatch(evFatal, ctx) || h.CurrentLeaf() != sDead || h.IsInState(sCombat) || ctx.combatExitCount != 1 {
		t.Fatalf("expected root bubble to Dead exiting Combat once")
	}
}

func TestBehaviorTreeAndTrainedMonsterAi(t *testing.T) {
	samples := []PairwiseTrainingSample{
		{Chosen: []int32{10000, 7000, 10000, 10000}, Rejected: []int32{0, 2000, 0, 0}, SampleWeight: 1.0},
		{Chosen: []int32{10000, 6000, 10000, 0}, Rejected: []int32{0, 3000, 0, 0}, SampleWeight: 1.0},
		{Chosen: []int32{0, 8500, 10000, 10000}, Rejected: []int32{0, 1500, 10000, 0}, SampleWeight: 1.0},
		{Chosen: []int32{10000, 5000, 10000, 0}, Rejected: []int32{0, 5000, 0, 0}, SampleWeight: 1.0},
		{Chosen: []int32{0, 9000, 0, 10000}, Rejected: []int32{0, 1000, 0, 0}, SampleWeight: 1.0},
		{Chosen: []int32{10000, 4000, 10000, 0}, Rejected: []int32{0, 4000, 10000, 0}, SampleWeight: 1.0},
	}

	model, metrics := TrainPairwiseLinearModel(4, samples, DefaultPairwiseTrainConfig())
	if !(metrics.FinalLoss < metrics.InitialLoss) || metrics.AccuracyPermyriad != 10000 {
		t.Fatalf("unexpected training metrics: %+v", metrics)
	}

	candidates := []ActionCandidate{
		{ActionID: 101, TargetID: 2001, FormationSlot: 0, Features: []int32{0, 1000, 0, 0}},
		{ActionID: 202, TargetID: 2002, FormationSlot: 1, Features: []int32{10000, 7500, 10000, 10000}},
	}
	pick, ok := SelectBestCandidate(candidates, model)
	if !ok || pick.Index != 1 {
		t.Fatalf("expected index 1, got %+v ok=%v", pick, ok)
	}

	tied := []ActionCandidate{
		{ActionID: 101, TargetID: 3002, FormationSlot: 2, Features: []int32{5000, 5000, 5000, 0}},
		{ActionID: 101, TargetID: 3001, FormationSlot: 1, Features: []int32{5000, 5000, 5000, 0}},
	}
	tiedPick, ok := SelectBestCandidate(tied, model)
	if !ok || tiedPick.Index != 1 {
		t.Fatalf("expected tie-break by formationSlot asc -> index 1, got %+v", tiedPick)
	}

	const (
		bbLastAction uint32 = 1
		bbLastTarget uint32 = 2
	)
	type monsterCtx struct {
		emergencyShieldNeeded bool
		channelTicksRemaining int
		haltCount             int
		shieldCasts           int
	}

	b := NewBehaviorTreeBuilder[*monsterCtx]()
	shieldBranch := b.Sequence(
		b.Condition(func(m *monsterCtx, _ *Blackboard) bool { return m.emergencyShieldNeeded }),
		b.Cooldown(3, b.Action(func(m *monsterCtx, bb *Blackboard) BtStatus {
			m.shieldCasts++
			bb.SetUint64(bbLastAction, 900)
			return BtSuccess
		}, nil)),
	)
	channelBranch := b.Sequence(
		b.Action(
			func(m *monsterCtx, _ *Blackboard) BtStatus {
				if m.channelTicksRemaining > 1 {
					m.channelTicksRemaining--
					return BtRunning
				}
				m.channelTicksRemaining = 2
				return BtSuccess
			},
			func(m *monsterCtx, _ *Blackboard) {
				m.haltCount++
				m.channelTicksRemaining = 2
			},
		),
		b.Action(func(_ *monsterCtx, bb *Blackboard) BtStatus {
			chosen, ok := SelectBestCandidate(candidates, model)
			if !ok {
				return BtFailure
			}
			bb.SetUint64(bbLastAction, uint64(candidates[chosen.Index].ActionID))
			bb.SetUint64(bbLastTarget, candidates[chosen.Index].TargetID)
			return BtSuccess
		}, nil),
	)
	root := b.ReactiveSelector(shieldBranch, channelBranch)
	tree, ok := b.Build(root)
	if !ok {
		t.Fatalf("expected valid acyclic tree")
	}

	mobA := &monsterCtx{channelTicksRemaining: 2}
	mobB := &monsterCtx{channelTicksRemaining: 2}
	stateA := tree.CreateState()
	stateB := tree.CreateState()

	if tree.Tick(mobA, &stateA) != BtRunning || tree.Tick(mobB, &stateB) != BtRunning {
		t.Fatalf("expected both mobs running on tick 1")
	}
	mobA.emergencyShieldNeeded = true
	if tree.Tick(mobA, &stateA) != BtSuccess || mobA.shieldCasts != 1 || mobA.haltCount != 1 ||
		stateA.Blackboard.GetUint64Or(bbLastAction, 0) != 900 {
		t.Fatalf("expected mobA preemptive shield with halt")
	}
	if tree.Tick(mobB, &stateB) != BtSuccess || mobB.haltCount != 0 ||
		stateB.Blackboard.GetUint64Or(bbLastAction, 0) != 202 ||
		stateB.Blackboard.GetUint64Or(bbLastTarget, 0) != 2002 {
		t.Fatalf("expected mobB uninterrupted AI decision")
	}
	if tree.Tick(mobA, &stateA) != BtRunning || mobA.shieldCasts != 1 {
		t.Fatalf("expected mobA shield on cooldown falling back to channel branch")
	}
}

func TestDungeonMonsterAiControllerLifecycle(t *testing.T) {
	profile := BuildTrainedDefaultMonsterAiProfile()
	ctrl := NewDungeonMonsterAiController(profile)
	if ctrl.CurrentPhase() != BossPhaseOpening || ctrl.CombatEnterCount() != 1 || ctrl.CombatExitCount() != 0 {
		t.Fatalf("unexpected initial boss phase")
	}

	makeInput := func(round, hpPermyriad uint32) *MonsterDecisionInput {
		return &MonsterDecisionInput{
			RoundIndex:      round,
			SelfHPPermyriad: hpPermyriad,
			SelfAlive:       true,
			SelfCanDefend:   true,
			Candidates: []MonsterActionCandidate{
				{
					Action:        MonsterBattleAction{ActionType: MonsterActionAttack, TargetID: 9101},
					FormationSlot: 0,
					Features:      [MonsterFeatureCount]int32{5000, 10000, 5000, 0, 0, 10000},
				},
			},
		}
	}

	// Round 1: 50% HP -> Opening -> Enrage，共享父态 Combat 不退出不重入。
	a1, ok := ctrl.DecideAction(makeInput(1, 5000))
	if !ok || a1.ActionType != MonsterActionAttack || ctrl.CurrentPhase() != BossPhaseEnrage ||
		ctrl.CombatEnterCount() != 1 || ctrl.CombatExitCount() != 0 {
		t.Fatalf("unexpected round 1 decision/phase: %+v phase=%v", a1, ctrl.CurrentPhase())
	}

	// Round 2: 20% HP -> Enrage -> Desperate，高优分支触发 DEFEND。
	a2, ok := ctrl.DecideAction(makeInput(2, 2000))
	if !ok || a2.ActionType != MonsterActionDefend || ctrl.CurrentPhase() != BossPhaseDesperate ||
		ctrl.CombatEnterCount() != 1 || ctrl.CombatExitCount() != 0 {
		t.Fatalf("unexpected round 2 decision/phase: %+v phase=%v", a2, ctrl.CurrentPhase())
	}

	// Round 3: 防御冷却中，回落攻击。
	a3, ok := ctrl.DecideAction(makeInput(3, 2000))
	if !ok || a3.ActionType != MonsterActionAttack {
		t.Fatalf("expected fallback attack during defend cooldown, got %+v", a3)
	}

	// Round 5: 冷却结束 (2 + 3 = 5)，再次触发防御。
	a5, ok := ctrl.DecideAction(makeInput(5, 2000))
	if !ok || a5.ActionType != MonsterActionDefend {
		t.Fatalf("expected defend after cooldown, got %+v", a5)
	}

	// 阵亡事件冒泡至 Root -> Dead，退出 Combat 一次。
	ctrl.NotifyDead()
	if ctrl.CurrentPhase() != BossPhaseDead || ctrl.IsInCombat() || ctrl.CombatExitCount() != 1 {
		t.Fatalf("expected dead phase with single combat exit")
	}
}
