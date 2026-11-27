package authority

import (
	"context"
	"database/sql"
	"testing"

	cgpb "proto/content_guard"
	"shared/contentguard"

	"github.com/stretchr/testify/require"
)

// 世界消息平台没在先行时限里答复:待补审和消息同一个事务落库;一次尝试只能被认领一次,租期内不再认领,
// 过了租期认领下一次;有了结局不再扫到;次数用完返回 Exhausted;补审前已撤回的带 AlreadyRetracted;私聊不进待办。
func TestPlatformRecheckQueueTiDB(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	s.guard = contentguard.Func(func(context.Context, cgpb.TextScene, uint64, string) contentguard.Result {
		// 本地层打码后放行、平台没在先行时限里答复。
		return contentguard.Result{Verdict: contentguard.Masked, Masked: "** 原消息", PendingPlatformReview: true}
	})
	now := uint64(1_700_000_000_000)
	s.clock = func(context.Context, *sql.Tx) (uint64, error) { return now, nil }
	ctx := context.Background()

	first, err := s.Send(original(t, 11), sendRequest("recheck-1", 0))
	require.NoError(t, err)
	require.Zero(t, first.Tip)
	_, err = s.Send(original(t, 11), sendRequest("recheck-private", 22))
	require.NoError(t, err)

	tasks, err := s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "只有世界消息进待办")
	require.EqualValues(t, 11, tasks[0].SenderPlayerID)
	require.EqualValues(t, first.Message.SendTimeMs, tasks[0].AcceptedMs)
	require.Zero(t, tasks[0].Attempt)
	require.Equal(t, "广告 原消息", tasks[0].Content, "送平台的是请求里的原文,不是打码之后的显示原件")

	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Empty(t, tasks, "租期里不再认领")

	now += platformRecheckLeaseMs
	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.EqualValues(t, 1, tasks[0].Attempt, "过了租期认领下一次")

	require.NoError(t, s.FinishPlatformRecheck(ctx, 11, first.Message.SendTimeMs, RecheckPassed))
	require.NoError(t, s.FinishPlatformRecheck(ctx, 11, first.Message.SendTimeMs, RecheckGaveUp), "重复记结局是空操作")
	now += platformRecheckLeaseMs
	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Empty(t, tasks, "有了结局不再扫到")

	// 第二条:三次都没结局,第四次拿到的是 Exhausted,且不再认领。
	second, err := s.Send(original(t, 12), sendRequest("recheck-2", 0))
	require.NoError(t, err)
	for attempt := uint32(0); attempt < PlatformRecheckMaxAttempts; attempt++ {
		tasks, err = s.ClaimPlatformRechecks(ctx, 8)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		require.Equal(t, attempt, tasks[0].Attempt)
		require.False(t, tasks[0].Exhausted)
		now += platformRecheckLeaseMs
	}
	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.True(t, tasks[0].Exhausted)
	require.EqualValues(t, second.Message.SendTimeMs, tasks[0].AcceptedMs)
	require.NoError(t, s.FinishPlatformRecheck(ctx, 12, second.Message.SendTimeMs, RecheckGaveUp))

	// 第三条:补审之前被撤回了。
	third, err := s.Send(original(t, 13), sendRequest("recheck-3", 0))
	require.NoError(t, err)
	retracted, tip, err := s.RetractWorld(ctx, 13, third.Message.SendTimeMs, "gm")
	require.NoError(t, err)
	require.Zero(t, tip)
	require.True(t, retracted)
	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.True(t, tasks[0].AlreadyRetracted)

	// 超出回看窗口的待办不再扫到。
	now += platformRecheckWindowMs + platformRecheckLeaseMs
	tasks, err = s.ClaimPlatformRechecks(ctx, 8)
	require.NoError(t, err)
	require.Empty(t, tasks)
}
