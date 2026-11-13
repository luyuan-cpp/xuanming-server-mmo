package logic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"match/internal/authority"
	"match/internal/battlepods"
	pb "proto/match"
)

// 2026-11-11:原执行进程失效的提前作废(global_instance_loss.go)的 logic 层回归。复核记录器与可疑表都用假时钟驱动,
// 成局权威用替身,不起 TiDB、不连 battle、不依赖墙钟。与原实例的真实交互在 global_deadline_evidence_test.go,
// 锁内提交在 authority/instance_gone_test.go。

// fakeClock 是用例手动拨的时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---- 复核记录器 ----

// 一次「已不在」只是一次观察;同一个实例隔满复核间隔再观察到一次才算确认,两次不必同种;差 1 毫秒也不算。
func TestInstanceLossTrackerConfirmsOnlyAfterTheSettleInterval(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	require.Equal(t, instanceVerdictUnknown, tracker.observe("instance-a", instanceUndetermined))
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-a", instanceSeenGone), "第一次只记时刻")
	require.False(t, tracker.gone("instance-a"))
	clock.Advance(instanceGoneSettle - time.Millisecond)
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-a", instanceSeenGone), "差 1 毫秒不算")
	clock.Advance(time.Millisecond)
	require.Equal(t, instanceVerdictGone, tracker.observe("instance-a", instanceSeenGone))
	require.True(t, tracker.gone("instance-a"))
	require.Equal(t, instanceVerdictGone, tracker.observe("instance-a", instanceUndetermined), "确认之后问不到也还是已不在")
	require.False(t, tracker.gone("instance-b"), "按实例记,互不相干")
}

// 期间原实例亲口应答:清掉之前的观察,从头再来。期间只是没问到(黑洞)不清。
func TestInstanceLossTrackerAnswerResetsButUndeterminedDoesNot(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-a", instanceSeenGone))
	clock.Advance(instanceGoneSettle / 2)
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-a", instanceUndetermined), "没问到不清")
	clock.Advance(instanceGoneSettle / 2)
	require.Equal(t, instanceVerdictGone, tracker.observe("instance-a", instanceSeenGone))

	require.Equal(t, instanceVerdictPending, tracker.observe("instance-b", instanceSeenGone))
	clock.Advance(instanceGoneSettle / 2)
	require.Equal(t, instanceVerdictAnswered, tracker.observe("instance-b", instanceAnswered), "它还活着")
	clock.Advance(instanceGoneSettle)
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-b", instanceSeenGone), "应答之后的第一次重新计")
	require.False(t, tracker.gone("instance-b"))
}

// 确认不在的实例又亲口应答(两次观察都错了):撤回结论,之后重新计。没有实例号的成局不记账,永远不会确认。
func TestInstanceLossTrackerWithdrawsAConfirmationWhenTheInstanceAnswers(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	tracker.observe("instance-a", instanceSeenGone)
	clock.Advance(instanceGoneSettle)
	require.Equal(t, instanceVerdictGone, tracker.observe("instance-a", instanceSeenGone))
	require.Equal(t, instanceVerdictAnswered, tracker.observe("instance-a", instanceAnswered))
	require.False(t, tracker.gone("instance-a"))

	for range 3 {
		require.Equal(t, instanceVerdictUnknown, tracker.observe("", instanceSeenGone))
		clock.Advance(instanceGoneSettle)
	}
	require.False(t, tracker.gone(""))
}

// 提前作废的取证位:同一个实例同一时刻只有一局在取证;等复核时下一次排在第一次观察之后满复核间隔;
// 一直判不了时从 5 秒翻倍退避到 30 秒;原实例应答之后不设等待。
func TestInstanceLossTrackerSchedulesOneProbePerInstance(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	start := clock.Now()

	plan, _ := tracker.plan("instance-a")
	require.Equal(t, instancePlanProbe, plan)
	plan, until := tracker.plan("instance-a")
	require.Equal(t, instancePlanWait, plan, "取证位已被占住")
	require.Equal(t, start.Add(instanceLossBusyRetry), until)
	verdict, next := tracker.finishProbe("instance-a", instanceSeenGone)
	require.Equal(t, instanceVerdictPending, verdict)
	require.Equal(t, start.Add(instanceGoneSettle), next, "复核排在第一次观察之后满复核间隔")
	plan, until = tracker.plan("instance-a")
	require.Equal(t, instancePlanWait, plan)
	require.Equal(t, start.Add(instanceGoneSettle), until)

	clock.Advance(instanceGoneSettle)
	plan, _ = tracker.plan("instance-a")
	require.Equal(t, instancePlanProbe, plan)
	verdict, next = tracker.finishProbe("instance-a", instanceUndetermined)
	require.Equal(t, instanceVerdictPending, verdict, "复核间隔已过而这一次没问到")
	require.Equal(t, clock.Now().Add(instanceLossFastProbe), next, "按快节拍再试")
	clock.Advance(instanceLossFastProbe)
	plan, _ = tracker.plan("instance-a")
	require.Equal(t, instancePlanProbe, plan)
	verdict, _ = tracker.finishProbe("instance-a", instanceSeenGone)
	require.Equal(t, instanceVerdictGone, verdict)
	plan, _ = tracker.plan("instance-a")
	require.Equal(t, instancePlanAbort, plan, "确认之后同实例的各局直接作废,不再取证")

	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
		plan, _ = tracker.plan("instance-b")
		require.Equal(t, instancePlanProbe, plan)
		verdict, next = tracker.finishProbe("instance-b", instanceUndetermined)
		require.Equal(t, instanceVerdictUnknown, verdict)
		require.Equal(t, clock.Now().Add(want), next, "判不了时翻倍退避,封顶 30 秒")
		clock.Advance(want)
	}
	plan, _ = tracker.plan("instance-b")
	require.Equal(t, instancePlanProbe, plan)
	verdict, next = tracker.finishProbe("instance-b", instanceAnswered)
	require.Equal(t, instanceVerdictAnswered, verdict)
	require.Equal(t, clock.Now(), next, "原实例应答:同实例的其余各局可以立刻各问一次")
}

// 记录久未被问到就清掉,再有人问从第一次观察重新计;正在取证的不清。
func TestInstanceLossTrackerForgetsIdleInstances(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	tracker.observe("instance-a", instanceSeenGone)
	plan, _ := tracker.plan("instance-busy")
	require.Equal(t, instancePlanProbe, plan)
	clock.Advance(instanceLossRecordTTL + time.Minute)
	require.Equal(t, instanceVerdictPending, tracker.observe("instance-a", instanceSeenGone), "旧记录已清,这是新的第一次")
	plan, _ = tracker.plan("instance-busy")
	require.Equal(t, instancePlanWait, plan, "正在取证的记录不清")
}

// 截止作废经同一个记录器:第一次观察回 InstanceGonePending(截止裁决据此把下一次取证排在复核间隔之后),复核通过回
// InstanceGone;实例确认之后同实例的其余各局不再取证;原实例亲口应答清掉观察;没有实例号的成局判不了。
func TestInstanceLossTrackerDeadlineEvidenceSharesTheRecheck(t *testing.T) {
	clock := newFakeClock()
	tracker := newInstanceLossTracker(clock.Now)
	scripted, probes := authority.DeadlineEvidenceInstanceGone, 0
	probe := tracker.deadlineEvidence(func(context.Context, *pb.MatchFormationRecord) authority.DeadlineAbortEvidence {
		probes++
		return scripted
	})
	ctx := context.Background()
	first := &pb.MatchFormationRecord{BattleId: 1, TargetInstanceId: "instance-a"}
	second := &pb.MatchFormationRecord{BattleId: 2, TargetInstanceId: "instance-a"}

	require.Equal(t, authority.DeadlineEvidenceInstanceGonePending, probe(ctx, first))
	require.Equal(t, authority.DeadlineEvidenceInstanceGonePending, probe(ctx, second), "同实例的另一局共用第一次观察")
	clock.Advance(instanceGoneSettle)
	require.Equal(t, authority.DeadlineEvidenceInstanceGone, probe(ctx, first))
	require.Equal(t, 3, probes)
	require.Equal(t, authority.DeadlineEvidenceInstanceGone, probe(ctx, second))
	require.Equal(t, 3, probes, "实例已确认不在:不再向原地址取证")

	other := &pb.MatchFormationRecord{BattleId: 3, TargetInstanceId: "instance-b"}
	require.Equal(t, authority.DeadlineEvidenceInstanceGonePending, probe(ctx, other))
	scripted = authority.DeadlineEvidenceBattleStillHolds
	clock.Advance(instanceGoneSettle)
	require.Equal(t, authority.DeadlineEvidenceBattleStillHolds, probe(ctx, other), "原实例还持有房间:证据原样返回")
	scripted = authority.DeadlineEvidenceInstanceGone
	require.Equal(t, authority.DeadlineEvidenceInstanceGonePending, probe(ctx, other), "应答清掉了之前的观察,重新计")

	scripted = authority.DeadlineEvidenceUndetermined
	require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(ctx, &pb.MatchFormationRecord{BattleId: 4, TargetInstanceId: "instance-c"}))
	scripted = authority.DeadlineEvidenceNoRoom
	require.Equal(t, authority.DeadlineEvidenceNoRoom, probe(ctx, &pb.MatchFormationRecord{BattleId: 5, TargetInstanceId: "instance-c"}))

	scripted = authority.DeadlineEvidenceInstanceGone
	noInstance := &pb.MatchFormationRecord{BattleId: 6}
	for range 2 {
		require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(ctx, noInstance), "没有实例号可记:判不了,由硬上限兜底")
		clock.Advance(instanceGoneSettle)
	}
}

// ---- Pod 查询的短时复用 ----

func TestCachedPodGoneReusesAnAnswerBrieflyAndNeverCachesFailures(t *testing.T) {
	clock := newFakeClock()
	calls, gone, failure := 0, true, error(nil)
	cached := newCachedPodGone(func(context.Context, string) (bool, error) {
		calls++
		return gone, failure
	}, clock.Now)
	ctx := context.Background()

	require.True(t, cached(ctx, "10.0.0.9:7001"))
	require.True(t, cached(ctx, "10.0.0.9:7002"), "同一个 IP 复用结论")
	require.Equal(t, 1, calls)
	gone = false
	require.False(t, cached(ctx, "10.0.0.10:7001"), "别的 IP 另查")
	require.Equal(t, 2, calls)
	clock.Advance(battlePodAnswerTTL)
	require.False(t, cached(ctx, "10.0.0.9:7001"), "过了复用期重新查")
	require.Equal(t, 3, calls)

	clock.Advance(battlePodAnswerTTL)
	failure = errors.New("fixture: forbidden")
	require.False(t, cached(ctx, "10.0.0.9:7001"), "查询失败按判不了")
	require.False(t, cached(ctx, "10.0.0.9:7001"))
	require.Equal(t, 5, calls, "失败不复用")
	failure = battlepods.ErrNotConfigured
	require.False(t, cached(ctx, "10.0.0.9:7001"))
	require.False(t, cached(ctx, "not-an-endpoint"), "地址拆不出主机:不查")
	require.Equal(t, 6, calls)

	require.Nil(t, newBattlePodGone("", clock.Now), "没配命名空间:不装配 Pod 查询")
}

// ---- 可疑表与取证循环 ----

// instanceLossStoreFake 是成局权威的替身:按 battle_id 给成局,记下作废的局。
type instanceLossStoreFake struct {
	mu           sync.Mutex
	formations   map[uint64]*pb.MatchFormationRecord
	settled      map[uint64]bool // 已有终局行
	candidateErr error
	abortErr     error
	aborted      []uint64
	proofs       []authority.InstanceGoneProof
}

func newInstanceLossStoreFake() *instanceLossStoreFake {
	return &instanceLossStoreFake{formations: map[uint64]*pb.MatchFormationRecord{}, settled: map[uint64]bool{}}
}

func (s *instanceLossStoreFake) add(battleID uint64, instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.formations[battleID] = &pb.MatchFormationRecord{BattleId: battleID, OperationId: "op", State: 4, TargetInstanceId: instance, TargetEndpoint: "10.0.0.9:7001"}
}

func (s *instanceLossStoreFake) InstanceLossCandidate(_ context.Context, battleID uint64) (*pb.MatchFormationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidateErr != nil {
		return nil, false, s.candidateErr
	}
	f, ok := s.formations[battleID]
	if !ok {
		return nil, false, nil
	}
	return f, !s.settled[battleID], nil
}

func (s *instanceLossStoreFake) AbortInstanceGone(_ context.Context, f *pb.MatchFormationRecord, proof authority.InstanceGoneProof) (pb.BattleOutcomeDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abortErr != nil {
		return 0, s.abortErr
	}
	s.settled[f.BattleId] = true
	s.aborted = append(s.aborted, f.BattleId)
	s.proofs = append(s.proofs, proof)
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}

func (s *instanceLossStoreFake) abortedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.aborted)
}

func (s *instanceLossStoreFake) setErrors(candidate, abort error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.candidateErr, s.abortErr = candidate, abort
}

// instanceLossRig 是一套假时钟驱动的可疑表:observe 按实例给脚本好的结论并计数,hold 记下存储错误。
type instanceLossRig struct {
	clock    *fakeClock
	store    *instanceLossStoreFake
	watcher  *instanceLossWatcher
	mu       sync.Mutex
	sighting map[string]instanceSighting
	observed int
	held     []error
	panics   int // 接下来几次 observe 直接 panic
}

func newInstanceLossRig() *instanceLossRig {
	rig := &instanceLossRig{clock: newFakeClock(), store: newInstanceLossStoreFake(), sighting: map[string]instanceSighting{}}
	observe := func(_ context.Context, f *pb.MatchFormationRecord) instanceSighting {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.observed++
		if rig.panics > 0 {
			rig.panics--
			panic("fixture: observe panics")
		}
		return rig.sighting[f.GetTargetInstanceId()]
	}
	hold := func(err error) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.held = append(rig.held, err)
	}
	rig.watcher = newInstanceLossWatcher(rig.store, observe, newInstanceLossTracker(rig.clock.Now), hold, rig.clock.Now)
	rig.watcher.Round(context.Background()) // 起过一轮之后才收可疑局
	return rig
}

func (r *instanceLossRig) see(instance string, sighting instanceSighting) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sighting[instance] = sighting
}

func (r *instanceLossRig) observations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.observed
}

func (r *instanceLossRig) holds() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.held)
}

func (r *instanceLossRig) suspects() int {
	r.watcher.mu.Lock()
	defer r.watcher.mu.Unlock()
	return len(r.watcher.suspects)
}

// tick 拨钟再跑一轮。
func (r *instanceLossRig) tick(d time.Duration) {
	r.clock.Advance(d)
	r.watcher.Round(context.Background())
}

// 没起取证循环的进程不收可疑局(表只进不出);起过之后同一局只记一次;表满了新来的不进。
func TestInstanceLossWatcherAcceptsSuspectsOnlyWhileItsLoopRuns(t *testing.T) {
	clock := newFakeClock()
	idle := newInstanceLossWatcher(newInstanceLossStoreFake(), nil, newInstanceLossTracker(clock.Now), func(error) {}, clock.Now)
	idle.Suspect(1)
	require.Empty(t, idle.suspects, "没起循环:不收")
	var missing *instanceLossWatcher
	missing.Suspect(1) // 没装配:不 panic

	rig := newInstanceLossRig()
	rig.watcher.Suspect(0)
	rig.watcher.Suspect(7)
	rig.watcher.Suspect(7)
	require.Equal(t, 1, rig.suspects())
	for id := uint64(100); rig.suspects() < instanceLossMaxSuspects; id++ {
		rig.watcher.Suspect(id)
	}
	rig.watcher.Suspect(1)
	require.Equal(t, instanceLossMaxSuspects, rig.suspects(), "表满了新来的不进")
}

// 一台 battle 崩溃、上面 300 局都有人补签:整个实例只取证两次(第一次观察、满复核间隔后的复核),复核之前一局都不作废,
// 复核通过后 300 局全部作废并撤出可疑表;证据带的是各局自己的原操作与原实例。
func TestInstanceLossWatcherAbortsEveryBattleOfAGoneInstanceWithTwoProbes(t *testing.T) {
	rig := newInstanceLossRig()
	const battles = 300
	for id := uint64(1); id <= battles; id++ {
		rig.store.add(id, "crashed-instance")
		rig.watcher.Suspect(id)
	}
	rig.see("crashed-instance", instanceSeenGone)

	rig.tick(0)
	require.Equal(t, 1, rig.observations(), "同一个实例只由一局取证")
	for range 4 {
		rig.tick(instanceLossBusyRetry)
	}
	rig.tick(instanceGoneSettle - 5*time.Second)
	require.Equal(t, 1, rig.observations(), "复核间隔没到:不再取证")
	require.Zero(t, rig.store.abortedCount(), "复核之前一局都不作废")
	require.Equal(t, battles, rig.suspects())

	rig.tick(time.Second) // 距第一次观察正好满复核间隔
	rig.tick(instanceLossBusyRetry)
	require.Equal(t, 2, rig.observations(), "复核只取证一次,其余各局直接作废")
	require.Equal(t, battles, rig.store.abortedCount())
	require.Zero(t, rig.suspects())
	require.Zero(t, rig.holds())
	for _, proof := range rig.store.proofs {
		require.Equal(t, authority.InstanceGoneProof{OperationId: "op", TargetInstanceId: "crashed-instance"}, proof)
	}

	// 实例确认之后才有人补签的局:不再取证,下一轮直接作废。
	rig.store.add(9001, "crashed-instance")
	rig.watcher.Suspect(9001)
	rig.tick(0)
	require.Equal(t, 2, rig.observations())
	require.Equal(t, battles+1, rig.store.abortedCount())
}

// 原实例还活着(一次网络抖动让补签失败):问到就撤出,不作废。已有终局、成局不存在、没有实例号的局同样直接撤出,不取证。
func TestInstanceLossWatcherReleasesBattlesThatAreNotItsBusiness(t *testing.T) {
	rig := newInstanceLossRig()
	rig.store.add(1, "alive-instance")
	rig.store.add(2, "alive-instance")
	rig.see("alive-instance", instanceAnswered)
	rig.watcher.Suspect(1)
	rig.watcher.Suspect(2)
	rig.tick(0)
	rig.tick(instanceLossBusyRetry)
	require.Equal(t, 2, rig.observations(), "各问一次")
	require.Zero(t, rig.suspects())
	require.Zero(t, rig.store.abortedCount())

	rig.store.add(3, "crashed-instance")
	rig.store.settled[3] = true // 另一条路径已经写了终局
	rig.store.add(4, "")        // 坏数据:没有实例号
	for _, id := range []uint64{3, 4, 5} {
		rig.watcher.Suspect(id) // 5 没有成局
	}
	rig.tick(0)
	require.Equal(t, 2, rig.observations(), "这三局都不取证")
	require.Zero(t, rig.suspects())
	require.Zero(t, rig.store.abortedCount())
}

// 一直判不了(原 IP 是黑洞、又没有 Pod 证据):留在表里按退避再问,不作废;之后原实例恢复应答就撤出。
func TestInstanceLossWatcherKeepsUndeterminedBattlesWithBackoff(t *testing.T) {
	rig := newInstanceLossRig()
	rig.store.add(1, "blackholed-instance")
	rig.watcher.Suspect(1)
	rig.tick(0)
	require.Equal(t, 1, rig.observations())
	rig.tick(instanceLossFastProbe - time.Second)
	require.Equal(t, 1, rig.observations(), "退避没到")
	rig.tick(time.Second)
	require.Equal(t, 2, rig.observations())
	rig.tick(2 * instanceLossFastProbe)
	require.Equal(t, 3, rig.observations())
	require.Equal(t, 1, rig.suspects())
	require.Zero(t, rig.store.abortedCount())

	rig.see("blackholed-instance", instanceAnswered)
	rig.tick(instanceLossSlowProbe)
	require.Zero(t, rig.suspects())
	require.Zero(t, rig.store.abortedCount())
}

// 存储失败时这一局留在表里按快节拍再试,恢复之后照常作废。读成局失败没有副作用,不记排空 HOLD;
// 作废事务失败(提交结果可能未知)才记。
func TestInstanceLossWatcherRetriesAfterStoreFailures(t *testing.T) {
	rig := newInstanceLossRig()
	rig.store.add(1, "crashed-instance")
	rig.see("crashed-instance", instanceSeenGone)
	rig.watcher.Suspect(1)
	failure := errors.New("fixture: tidb unavailable")

	rig.store.setErrors(failure, nil)
	rig.tick(0)
	require.Zero(t, rig.holds(), "只读失败不记 HOLD")
	require.Zero(t, rig.observations(), "成局没读到:不取证")
	require.Equal(t, 1, rig.suspects())
	rig.store.setErrors(nil, nil)
	rig.tick(instanceLossFastProbe)
	require.Equal(t, 1, rig.observations())

	rig.store.setErrors(nil, failure)
	rig.tick(instanceGoneSettle)
	require.Equal(t, 2, rig.observations(), "复核通过")
	require.Equal(t, 1, rig.holds(), "作废事务失败:提交结果可能未知")
	require.Equal(t, 1, rig.suspects())
	rig.store.setErrors(nil, nil)
	rig.tick(instanceLossFastProbe)
	require.Equal(t, 1, rig.store.abortedCount(), "实例已确认:恢复之后直接作废,不再取证")
	require.Equal(t, 2, rig.observations())
	require.Zero(t, rig.suspects())
}

// 取证 panic:这一局不丢、取证位交回(否则同实例的各局会一直等),下一拍照常取证。
func TestInstanceLossWatcherSurvivesAPanickingProbe(t *testing.T) {
	rig := newInstanceLossRig()
	rig.store.add(1, "crashed-instance")
	rig.store.add(2, "crashed-instance")
	rig.see("crashed-instance", instanceSeenGone)
	rig.mu.Lock()
	rig.panics = 1
	rig.mu.Unlock()
	rig.watcher.Suspect(1)
	rig.watcher.Suspect(2)

	rig.tick(0)
	require.Equal(t, 2, rig.suspects(), "panic 的那一局留在表里")
	rig.tick(instanceLossFastProbe)
	rig.tick(instanceLossBusyRetry)
	require.GreaterOrEqual(t, rig.observations(), 2, "取证位已交回,之后又取证了")
	rig.tick(instanceGoneSettle)
	rig.tick(instanceLossBusyRetry)
	require.Equal(t, 2, rig.store.abortedCount())
	require.Zero(t, rig.suspects())
}
