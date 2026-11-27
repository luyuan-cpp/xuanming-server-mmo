package authority

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"chat/internal/constants"
	chatpb "proto/chat"
	"shared/authoritysql"

	"github.com/stretchr/testify/require"
)

func TestNormalizeSpamHashFoldsPunctAndWidthAndSkipsShortText(t *testing.T) {
	h1, n1, ok1 := normalizeSpamHash("加 微 信：A B １２３！")
	h2, n2, ok2 := normalizeSpamHash("加微信 ab123")
	require.True(t, ok1)
	require.True(t, ok2)
	require.Equal(t, h1, h2, "分隔符、标点、全半角与大小写变换后应命中同一刷屏摘要")
	require.Equal(t, 8, n1, "归一化后的字符数不含分隔符与标点")
	require.Equal(t, n1, n2)

	_, _, okShort := normalizeSpamHash("6 6 6")
	require.False(t, okShort, "不足 4 个有效字符的短语(如 +1 / 666)不计入重复刷屏")
}

func TestUtcDayYMDAndFilterRetractedRows(t *testing.T) {
	day := utcDayYMD(uint64(time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC).UnixMilli()))
	require.EqualValues(t, 20261120, day)

	rows := []historyRow{
		{sender: 1, acceptedMs: 100},
		{sender: 2, acceptedMs: 200},
		{sender: 3, acceptedMs: 300},
	}
	retracted := map[retractedKey]struct{}{{sender: 2, acceptedMs: 200}: {}}
	out := filterRetractedRows(rows, retracted)
	require.Len(t, out, 2)
	require.EqualValues(t, 1, out[0].sender)
	require.EqualValues(t, 3, out[1].sender)
}

func TestProfileCacheRespectsTTLAndBounds(t *testing.T) {
	var now atomic.Int64
	var calls atomic.Int32
	cache := newProfileCache(10*time.Second, 2, func() time.Duration { return time.Duration(now.Load()) })
	reader := ProfileReaderFunc(func(_ context.Context, id uint64) (PlayerProfile, error) {
		calls.Add(1)
		return PlayerProfile{Level: uint32(id)}, nil
	})
	p, err := cache.get(context.Background(), reader, 7)
	require.NoError(t, err)
	require.EqualValues(t, 7, p.Level)
	p, err = cache.get(context.Background(), reader, 7)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())

	now.Add(int64(11 * time.Second))
	_, err = cache.get(context.Background(), reader, 7)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

func TestAntiSpamRetractAndReportTiDB(t *testing.T) {
	baseStore, fixture := nativeStoreFixture(t, false)
	ctx := context.Background()
	var schema string
	require.NoError(t, baseStore.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema))
	db, err := fixture.Open(ctx, schema, 4)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var nowMs uint64 = 1_700_000_000_000
	rules := fixtureRules()
	rules.WorldMinLevel = 5
	rules.DuplicateSpamWindowMs = 60_000
	rules.DuplicateSpamMaxSenders = 2
	rules.DuplicateSpamMaxPerSender = 2
	rules.NewAccountAgeHours = 24
	rules.NewAccountDailyPrivateLimit = 2

	profiles := map[uint64]PlayerProfile{
		11: {Level: 3, AccountCreatedMs: nowMs - 1000},          // 新号低等级
		12: {Level: 10, AccountCreatedMs: nowMs - 1000},         // 新号满级
		13: {Level: 10, AccountCreatedMs: nowMs - 48*3600*1000}, // 老号满级
		14: {Level: 10, AccountCreatedMs: nowMs - 48*3600*1000}, // 老号满级
	}
	store, err := New(ctx, db, baseStore.states, authoritysql.BackendTiDB, rules, WithProfileReader(ProfileReaderFunc(func(_ context.Context, id uint64) (PlayerProfile, error) {
		return profiles[id], nil
	})))
	require.NoError(t, err)
	// 时间接缝:每取一次时间前进 1.5 秒。新号判定、每秒限流、重复刷屏的时间窗都按这个时间算(档案里的建号时间也是按它写的),
	// 用库的真实时间的话「新号」早就不是新号了;每条消息落在不同的秒里,不撞每秒限流,同一个人的两条相同内容也不会落在同一毫秒。
	clockMs := nowMs
	store.clock = func(context.Context, *sql.Tx) (uint64, error) {
		clockMs += 1500
		return clockMs, nil
	}

	// 1. 世界频道等级门槛。
	res, err := store.Send(original(t, 11), &chatpb.SendChatRequest{
		RequestId: "lvl-low",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "正常世界发言"},
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrLevelTooLow, res.Tip)

	// 2. 新号每日私聊人数上限(最多 2 名不同对端)。
	for i, peer := range []uint64{101, 102} {
		r, sendErr := store.Send(original(t, 12), &chatpb.SendChatRequest{
			RequestId: "priv-" + string(rune('a'+i)),
			Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, TargetPlayerId: peer, Content: "你好呀对端" + string(rune('a'+i))},
		})
		require.NoError(t, sendErr)
		require.Zero(t, r.Tip)
	}
	// 已聊过的 101 仍可续聊,新第 3 人 103 被拒。
	rRepeatPeer, err := store.Send(original(t, 12), &chatpb.SendChatRequest{
		RequestId: "priv-repeat-101",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, TargetPlayerId: 101, Content: "继续和你聊一聊"},
	})
	require.NoError(t, err)
	require.Zero(t, rRepeatPeer.Tip)

	rOverPeer, err := store.Send(original(t, 12), &chatpb.SendChatRequest{
		RequestId: "priv-over-103",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, TargetPlayerId: 103, Content: "骚扰第三位"},
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrPrivateTargetLimit, rOverPeer.Tip)

	// 3. 多账号跨副本重复刷屏(同一归一化内容 2 个发送者放行,第 3 个发送者拒绝)。
	firstWorld, err := store.Send(original(t, 12), &chatpb.SendChatRequest{
		RequestId: "spam-12",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "限时特惠代练加群先到先得"},
	})
	require.NoError(t, err)
	require.Zero(t, firstWorld.Tip)
	secondWorld, err := store.Send(original(t, 13), &chatpb.SendChatRequest{
		RequestId: "spam-13",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "限 时 特 惠 代 练 加 群,先 到 先 得!"},
	})
	require.NoError(t, err)
	require.Zero(t, secondWorld.Tip)
	thirdWorld, err := store.Send(original(t, 14), &chatpb.SendChatRequest{
		RequestId: "spam-14",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "限时特惠代练加群先到先得"},
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrDuplicateSpam, thirdWorld.Tip)

	// 3b. 短句不按人数判:已经有两个人说过「哈哈哈哈哈」,第三个人照样能说。
	for i, sender := range []uint64{12, 13, 14} {
		short, sendErr := store.Send(original(t, sender), &chatpb.SendChatRequest{
			RequestId: "short-" + string(rune('a'+i)),
			Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "哈哈哈哈哈"},
		})
		require.NoError(t, sendErr)
		require.Zero(t, short.Tip, "a short phrase from a third sender must not be rejected")
	}
	// 3c. 同一个人重复发同一段内容:前两条放行,第三条拒绝(短句也算)。
	second, err := store.Send(original(t, 13), &chatpb.SendChatRequest{
		RequestId: "short-again-1",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "哈哈哈哈哈!"},
	})
	require.NoError(t, err)
	require.Zero(t, second.Tip)
	third, err := store.Send(original(t, 13), &chatpb.SendChatRequest{
		RequestId: "short-again-2",
		Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "哈哈哈哈哈"},
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrDuplicateSpam, third.Tip)
	// 3d. 私聊不判重复:同一句话发三遍、别人也说过,都不拦(13、14 是老号,不受私聊人数上限约束)。
	for i := 0; i < 3; i++ {
		private, sendErr := store.Send(original(t, 13), &chatpb.SendChatRequest{
			RequestId: "priv-same-" + string(rune('a'+i)),
			Message:   &chatpb.ChatMessage{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, TargetPlayerId: 14, Content: "限时特惠代练加群先到先得"},
		})
		require.NoError(t, sendErr)
		require.Zero(t, private.Tip, "private chat is not subject to the duplicate-content rule")
	}

	// 4. 举报已发送的世界消息并幂等重入。
	reportTip, err := store.ReportMessage(original(t, 13), &chatpb.ReportChatRequest{
		Channel:              chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		TargetSenderPlayerId: firstWorld.Message.SenderPlayerId,
		SendTimeMs:           firstWorld.Message.SendTimeMs,
		Reason:               1,
		Detail:               "疑似广告",
	})
	require.NoError(t, err)
	require.Zero(t, reportTip)

	missingTip, err := store.ReportMessage(original(t, 13), &chatpb.ReportChatRequest{
		Channel:              chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		TargetSenderPlayerId: 12,
		SendTimeMs:           1,
		Reason:               1,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrMessageNotFound, missingTip)

	// 5. 撤回世界消息后 Pull 历史不再可见。
	retracted, tip, err := store.RetractWorld(ctx, firstWorld.Message.SenderPlayerId, firstWorld.Message.SendTimeMs, "platform_async_reject")
	require.NoError(t, err)
	require.Zero(t, tip)
	require.True(t, retracted)
	worldHist := pull(t, store, original(t, 13), 0)
	for _, m := range worldHist {
		require.False(t, m.SenderPlayerId == firstWorld.Message.SenderPlayerId && m.SendTimeMs == firstWorld.Message.SendTimeMs)
	}
}
