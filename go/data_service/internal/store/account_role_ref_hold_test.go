package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 后台判陈旧留下的建角 ref(角色行不在,却有提交过的证据)标成保留之后不再占扫描名额(account_role_ref.go 的 SweepStaleRefs):
// 只在复核名额里按 held_ms 轮着重判;人把角色行补回来、或者证据没了,轮到它的那一轮就收掉。目录各步用 flowSteps。

func agedCreateRef(player uint64) roleRef {
	return roleRef{PlayerID: player, State: refStatePending, HomeZone: 1, AgeMs: uint64(refPendingGrace.Milliseconds())}
}

// 用例「被留下的 ref 不再占名额」:积压一批留下的 ref,新来的陈旧 ref 照样在本轮扫到;留下的不再进扫描,
// 只按复核名额(heldRefRecheckBatch)复核,也不再重复计 Held。
func TestHeldRefsDoNotTakeSweepSlots(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.evidence = map[uint64]string{}
	const backlog = 3 * heldRefRecheckBatch
	for player := uint64(1); player <= backlog; player++ {
		steps.evidence[player] = committedEvidenceNameBound
		steps.putRef("held", agedCreateRef(player))
	}

	// 第一轮名额只有 4:先扫到的 4 条标成保留。
	result, err := s.SweepStaleRefs(ctx, refPendingGrace, 4)
	require.NoError(t, err)
	require.Equal(t, 4, result.Scanned)
	require.Equal(t, 4, result.Held)
	require.Equal(t, 4, result.HeldTotal)

	// 把剩下的也都标完。
	for result.HeldTotal < backlog {
		result, err = s.SweepStaleRefs(ctx, refPendingGrace, 4)
		require.NoError(t, err)
	}

	// 积压之后来了一条没有证据的陈旧 ref:名额只有 1,也轮得到它,留下的那一批不再进扫描。
	steps.putRef("late", agedCreateRef(900))
	steps.calls = nil
	result, err = s.SweepStaleRefs(ctx, refPendingGrace, 1)
	require.NoError(t, err)
	require.Equal(t, 1, result.Scanned, "扫描名额只给了新来的那一条")
	require.Equal(t, 1, result.Dropped)
	require.NotContains(t, steps.refs["late"], uint64(900), "新来的陈旧 ref 本轮就清掉了")
	require.Zero(t, result.Held, "已保留的不再重复计")
	require.Equal(t, heldRefRecheckBatch, result.Rechecked, "复核单独一份小名额")
	require.Equal(t, backlog, result.HeldTotal)
	require.True(t, result.HeldCounted)
	require.Equal(t, heldRefRecheckBatch+1, steps.count("committedEvidence"), "只重判复核名额里的和新来的那一条")
}

// 复核按 held_ms 从旧到新轮着取:复核过还留着的刷新 held_ms 排到队尾,几轮之后每一条都复核到。
func TestHeldRefRecheckRotatesThroughTheBacklog(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.evidence = map[uint64]string{}
	const backlog = 2 * heldRefRecheckBatch
	for player := uint64(1); player <= backlog; player++ {
		steps.evidence[player] = committedEvidenceStoreSaved
		steps.putRef("held", agedCreateRef(player))
	}
	_, err := s.SweepStaleRefs(ctx, refPendingGrace, backlog)
	require.NoError(t, err)

	steps.calls = nil
	for round := 0; round < 2; round++ {
		result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
		require.NoError(t, err)
		require.Equal(t, heldRefRecheckBatch, result.Rechecked)
	}
	require.Equal(t, backlog, steps.count("committedEvidence"), "两轮复核正好轮完一遍,没有哪一条被复核两次")
}

// 人把角色行补回来(真相就是这个账号):复核那一轮补 ACTIVE,保留标记一起清掉,计数降回去。
func TestHeldRefRecheckSettlesWhenTheRoleIsBack(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.evidence = map[uint64]string{11: committedEvidenceStoreSaved}
	steps.putRef("owner", agedCreateRef(11))
	result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, 1, result.HeldTotal)

	steps.setOwner(11, "owner", 1, 0) // 玩家片按更靠近 T 的点重新恢复,角色行回来了
	result, err = s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, 1, result.Rechecked)
	require.Equal(t, refStateActive, steps.refs["owner"][11].State, "补上 ACTIVE")
	require.Zero(t, steps.refs["owner"][11].HeldMs, "保留标记清掉")
	require.Zero(t, result.HeldTotal)
}

// 证据没了(人核对之后清掉了误留的痕迹):复核那一轮照常写墓碑、删 ref。
func TestHeldRefRecheckTombstonesWhenTheEvidenceIsGone(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.evidence = map[uint64]string{11: committedEvidenceStoreSaved}
	steps.putRef("owner", agedCreateRef(11))
	_, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)

	delete(steps.evidence, 11)
	result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, 1, result.Rechecked)
	require.Equal(t, 1, result.Dropped)
	require.NotContains(t, steps.refs["owner"], uint64(11))
	require.Equal(t, "", steps.owners[11].Account, "墓碑挡住迟到的建角")
	require.Zero(t, result.HeldTotal)
}

// 读路径发现的陈旧 ref 里有已保留的:跳过,不占扫描名额(它由复核名额管)。
func TestDrainSkipsHeldRefs(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.evidence = map[uint64]string{11: committedEvidenceNameBound}
	steps.putRef("owner", agedCreateRef(11))
	_, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)

	s.noteStaleRef("owner", 11)
	steps.calls = nil
	result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Empty(t, s.staleRefs, "排队的那一条取出来了")
	require.Zero(t, result.Scanned, "已保留的不进扫描名额")
	require.Equal(t, 1, result.Rechecked, "复核名额照常")
	require.Equal(t, 1, steps.count("committedEvidence"), "只在复核里判了一次")
}

// 复核与计数读失败不挡扫描:错误合起来回,这一轮的计数标成没数成,调用方不写 gauge。
func TestSweepReportsHeldCountFailures(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	steps.putRef("late", agedCreateRef(900))
	steps.fail["scanHeldRefs"] = []error{errFlow}
	steps.fail["countHeldRefs"] = []error{errFlow}
	result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.ErrorIs(t, err, errFlow)
	require.Equal(t, 1, result.Dropped, "扫描照常")
	require.False(t, result.HeldCounted)
}
