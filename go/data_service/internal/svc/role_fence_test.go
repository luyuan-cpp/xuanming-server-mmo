package svc

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"
	"shared/authoritysql"
	"shared/drainproof"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

type roleRemovalsFake struct {
	calls     int
	olderThan time.Duration
	limit     int
	result    store.RemovalResumeResult
	err       error
	panics    bool
}

func (f *roleRemovalsFake) ResumeRemovals(_ context.Context, olderThan time.Duration, limit int) (store.RemovalResumeResult, error) {
	f.calls++
	f.olderThan, f.limit = olderThan, limit
	if f.panics {
		panic("test")
	}
	return f.result, f.err
}

// counterValue 读默认注册表里某个计数在一组 label 下的当前值;还没有这条序列时是 0。计数是进程级累加的,用例只比增量。
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := len(m.GetLabel()) == len(labels)
			for _, pair := range m.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
				}
			}
			if matched {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// 用例「删角收尾失败不再算完成」:后台一轮的逐行结局都计,收尾没做完的计 unsettled,出错的一轮也照记。
func TestResumeRoleRemovalsCountsEveryResult(t *testing.T) {
	const name = "data_service_account_role_removal_resume_total"
	before := map[string]float64{}
	for _, result := range []string{"finished", "abandoned", "unsettled", "failed"} {
		before[result] = counterValue(t, name, map[string]string{"result": result})
	}
	fake := &roleRemovalsFake{result: store.RemovalResumeResult{Finished: 2, Abandoned: 1, Unsettled: 3}}
	s := &ServiceContext{RelayAdmission: drainproof.New(), RoleRemovals: fake}
	s.resumeRoleRemovals(context.Background())
	fake.result, fake.err = store.RemovalResumeResult{Unsettled: 1, Failed: 1}, errors.New("participant unreachable")
	s.resumeRoleRemovals(context.Background())

	for result, want := range map[string]float64{"finished": 2, "abandoned": 1, "unsettled": 4, "failed": 1} {
		require.Equalf(t, before[result]+want, counterValue(t, name, map[string]string{"result": result}), "result=%s", result)
	}
}

// 目录的失败计数接到 metrics:建角提交之后名字没绑上、删角收尾没做完,各按原因计一次。
func TestDirectoryEventsReachTheCounters(t *testing.T) {
	const bind, settle = "data_service_account_role_name_bind_failures_total", "data_service_account_role_removal_settle_failures_total"
	beforeBind := counterValue(t, bind, map[string]string{"reason": store.NameBindFailureTaken})
	beforeSettle := counterValue(t, settle, map[string]string{"step": store.RemovalSettleReleaseName})
	var events store.DirectoryEvents = directoryEvents{}
	events.NameBindFailed(store.NameBindFailureTaken)
	events.RemovalSettleFailed(store.RemovalSettleReleaseName)
	require.Equal(t, beforeBind+1, counterValue(t, bind, map[string]string{"reason": store.NameBindFailureTaken}))
	require.Equal(t, beforeSettle+1, counterValue(t, settle, map[string]string{"step": store.RemovalSettleReleaseName}))
}

func TestResumeRoleRemovalsGoesThroughRelayGate(t *testing.T) {
	fake := &roleRemovalsFake{}
	relay := drainproof.New()
	s := &ServiceContext{RelayAdmission: relay, RoleRemovals: fake}
	s.resumeRoleRemovals(context.Background())
	require.Equal(t, 1, fake.calls)
	require.Equal(t, 5*time.Minute, fake.olderThan, "规格 §3.5:超过 5 分钟才接着做")
	require.Equal(t, uint64(0), relay.Pending(), "一轮结束要退出门禁")
	relay.CloseAdmission()
	s.resumeRoleRemovals(context.Background())
	require.Equal(t, 1, fake.calls, "门禁关了不再写")
}

// 结果未知和 panic 都要让 RELAY 保持未知,排空不能宣称干净。
func TestResumeRoleRemovalsHoldsRelayOnUnknownOutcome(t *testing.T) {
	for name, fake := range map[string]*roleRemovalsFake{
		"unknown": {err: errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))},
		"panic":   {panics: true},
	} {
		t.Run(name, func(t *testing.T) {
			relay := drainproof.New()
			s := &ServiceContext{RelayAdmission: relay, RoleRemovals: fake}
			s.resumeRoleRemovals(context.Background())
			relay.CloseAdmission()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.Error(t, relay.Wait(ctx))
		})
	}
}

func TestRoleRemovalResumerStartAndStop(t *testing.T) {
	s := &ServiceContext{RelayAdmission: drainproof.New()}
	s.StartRoleRemovalResumer()
	require.NoError(t, s.StopRoleRemovalResumer(context.Background()), "协调者缺席时不起,停也不报错")
	s.RoleRemovals = &roleRemovalsFake{}
	s.StartRoleRemovalResumer()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.StopRoleRemovalResumer(ctx))
}

type roleRefsFake struct {
	calls     int
	olderThan time.Duration
	limit     int
	result    store.SweepResult
	err       error
	panics    bool
}

func (f *roleRefsFake) SweepStaleRefs(_ context.Context, olderThan time.Duration, limit int) (store.SweepResult, error) {
	f.calls++
	f.olderThan, f.limit = olderThan, limit
	if f.panics {
		panic("test")
	}
	return f.result, f.err
}

// roleRefHeldCount 读 account_role_ref_tombstone_held_total 的当前值;还没注册时是 0。计数是进程级累加的,用例只比增量。
func roleRefHeldCount(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == "data_service_account_role_ref_tombstone_held_total" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// 一轮里新标成保留、没写墓碑的建角 ref(角色行不在,但角色提交过)计进低基数计数,出错的一轮也照记;逐条明细在 store 的 ERROR 日志里。
// 还留着多少条报 gauge;这一轮没数成(HeldCounted=false)不写,保留上一轮的值。
func TestSweepStaleRoleRefsCountsHeldRefs(t *testing.T) {
	const gauge = "data_service_account_role_ref_held"
	before := roleRefHeldCount(t)
	s := &ServiceContext{RelayAdmission: drainproof.New(), RoleRefs: &roleRefsFake{result: store.SweepResult{Scanned: 3, Dropped: 1, Held: 2, HeldTotal: 5, HeldCounted: true}}}
	s.sweepStaleRoleRefs(context.Background())
	require.Equal(t, before+2, roleRefHeldCount(t))
	require.Equal(t, float64(5), counterValue(t, gauge, map[string]string{}))

	s.RoleRefs = &roleRefsFake{result: store.SweepResult{Scanned: 1, Held: 1}, err: errors.New("one ref failed")}
	s.sweepStaleRoleRefs(context.Background())
	require.Equal(t, before+3, roleRefHeldCount(t))
	require.Equal(t, float64(5), counterValue(t, gauge, map[string]string{}), "没数成就不写,保留上一轮的值")
}

func TestSweepStaleRoleRefsGoesThroughRelayGate(t *testing.T) {
	fake := &roleRefsFake{}
	relay := drainproof.New()
	s := &ServiceContext{RelayAdmission: relay, RoleRefs: fake}
	s.sweepStaleRoleRefs(context.Background())
	require.Equal(t, 1, fake.calls)
	require.Equal(t, roleRefSweepAfter, fake.olderThan)
	require.Equal(t, roleRefSweepBatch, fake.limit)
	require.Equal(t, uint64(0), relay.Pending(), "一轮结束要退出门禁")
	relay.CloseAdmission()
	s.sweepStaleRoleRefs(context.Background())
	require.Equal(t, 1, fake.calls, "门禁关了不再写")
}

// 结果未知和 panic 都要让 RELAY 保持未知,排空不能宣称干净。
func TestSweepStaleRoleRefsHoldsRelayOnUnknownOutcome(t *testing.T) {
	for name, fake := range map[string]*roleRefsFake{
		"unknown": {err: errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))},
		"panic":   {panics: true},
	} {
		t.Run(name, func(t *testing.T) {
			relay := drainproof.New()
			s := &ServiceContext{RelayAdmission: relay, RoleRefs: fake}
			s.sweepStaleRoleRefs(context.Background())
			relay.CloseAdmission()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.Error(t, relay.Wait(ctx))
		})
	}
}

type rolePreparingFake struct {
	calls  int
	limit  int
	err    error
	panics bool
}

func (f *rolePreparingFake) AbortStalePreparing(_ context.Context, limit int) (store.PrepareAbortResult, error) {
	f.calls++
	f.limit = limit
	if f.panics {
		panic("test")
	}
	return store.PrepareAbortResult{}, f.err
}

func TestAbortStaleRolePreparingGoesThroughRelayGate(t *testing.T) {
	fake := &rolePreparingFake{}
	relay := drainproof.New()
	s := &ServiceContext{RelayAdmission: relay, RolePreparing: fake}
	s.abortStaleRolePreparing(context.Background())
	require.Equal(t, 1, fake.calls)
	require.Equal(t, rolePrepareAbortBatch, fake.limit, "每轮有上限")
	require.Equal(t, uint64(0), relay.Pending(), "一轮结束要退出门禁")
	relay.CloseAdmission()
	s.abortStaleRolePreparing(context.Background())
	require.Equal(t, 1, fake.calls, "门禁关了不再写")
}

// 结果未知和 panic 都要让 RELAY 保持未知,排空不能宣称干净。普通失败留给下一轮,不是未知。
func TestAbortStaleRolePreparingHoldsRelayOnlyOnUnknownOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		fake *rolePreparingFake
		hold bool
	}{
		"unknown": {&rolePreparingFake{err: errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))}, true},
		"panic":   {&rolePreparingFake{panics: true}, true},
		"sql失败":   {&rolePreparingFake{err: errors.New("Error 1205")}, false},
		"这一轮没东西":  {&rolePreparingFake{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			relay := drainproof.New()
			s := &ServiceContext{RelayAdmission: relay, RolePreparing: tc.fake}
			s.abortStaleRolePreparing(context.Background())
			relay.CloseAdmission()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if tc.hold {
				require.Error(t, relay.Wait(ctx))
			} else {
				require.NoError(t, relay.Wait(ctx))
			}
		})
	}
}

// 没有协调者时也要清 ref:过户和删角停了,建角失败留下的 PENDING 照样占名额。
func TestBackgroundLoopRunsWithoutCoordinator(t *testing.T) {
	s := &ServiceContext{RelayAdmission: drainproof.New(), RoleRefs: &roleRefsFake{}}
	s.StartRoleRemovalResumer()
	require.NotNil(t, s.stopRemovalResumer)
	// 没有协调者时这一轮只清 ref,不碰删角收尾。
	s.resumeRoleRemovals(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.StopRoleRemovalResumer(ctx))
}
