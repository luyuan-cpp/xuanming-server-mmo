package authority

import (
	"fmt"
	"testing"
	"time"

	"chat/internal/constants"
	chatpb "proto/chat"

	"github.com/stretchr/testify/require"
)

// 世界频道近窗快照的真库用例:快照与直读逐条一致;别的副本的新消息最多滞后 maxAge;本副本自己的提交立刻可见。
func TestChatTiDBWorldWindowMatchesDirectReadAndBoundsStaleness(t *testing.T) {
	rules := fixtureRules()
	rules.RateLimitPerSecond = 100
	rules.WorldHistoryCacheMs = constants.MaxWorldHistoryCacheMs
	s, fixture := nativeStoreFixture(t, false, rules)
	clock := &fakeMonotonic{}
	s.world.now = clock.now // 快照年龄只由这里推进,用例不依赖真实时间

	// 直读的副本:同一个库、独立连接池,关掉快照。
	withWindow := s.rules
	s.rules.WorldHistoryCacheMs = 0
	direct := independentStore(t, s, fixture)
	s.rules = withWindow
	require.Nil(t, direct.world)

	ctx := original(t, 11)
	for i := range 3 {
		_, err := s.Send(ctx, sendRequest(fmt.Sprintf("world-%d", i), 0))
		require.NoError(t, err)
	}
	requireSameHistory(t, pull(t, direct, ctx, 0), pull(t, s, ctx, 0))
	require.Len(t, pull(t, s, ctx, 0), 3)

	_, err := direct.Send(ctx, sendRequest("from-other-replica", 0))
	require.NoError(t, err)
	require.Len(t, pull(t, direct, ctx, 0), 4)
	require.Len(t, pull(t, s, ctx, 0), 3, "别的副本刚提交的消息在 maxAge 内可以还没出现")

	clock.advance(time.Duration(constants.MaxWorldHistoryCacheMs) * time.Millisecond)
	requireSameHistory(t, pull(t, direct, ctx, 0), pull(t, s, ctx, 0))
	require.Len(t, pull(t, s, ctx, 0), 4, "满 maxAge 就重读,看到别的副本的提交")

	_, err = s.Send(ctx, sendRequest("own-commit", 0))
	require.NoError(t, err)
	got := pull(t, s, ctx, 0)
	require.Len(t, got, 5, "本副本自己的提交不等快照过期")
	requireSameHistory(t, pull(t, direct, ctx, 0), got)

	// 重放不改历史,也不作废快照之外的任何东西:重复的 request_id 不多出一条。
	r, err := s.Send(ctx, sendRequest("own-commit", 0))
	require.NoError(t, err)
	require.True(t, r.Duplicate)
	require.Len(t, pull(t, s, ctx, 0), 5)

	// 私聊不走快照,照旧直读。
	_, err = s.Send(ctx, sendRequest("private", 22))
	require.NoError(t, err)
	require.Len(t, pull(t, s, ctx, 22), 1)
}

func requireSameHistory(t *testing.T, want, got []*chatpb.ChatMessage) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].SenderPlayerId, got[i].SenderPlayerId, "第 %d 条发送者", i)
		require.Equal(t, want[i].SendTimeMs, got[i].SendTimeMs, "第 %d 条时间", i)
		require.Equal(t, want[i].Content, got[i].Content, "第 %d 条正文", i)
		require.Equal(t, want[i].Channel, got[i].Channel, "第 %d 条频道", i)
	}
}
