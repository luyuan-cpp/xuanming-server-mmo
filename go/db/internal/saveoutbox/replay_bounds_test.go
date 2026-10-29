package saveoutbox

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"shared/savebarrier"
)

// 补投有界(审计 go-db-pipeline#1):首见宽限、指数退避与次数上限、终态停止、在途重试推迟、主库冷却与 legacy 存活整拍跳过。
// 时间全部经 Replay.now 注入,抖动置零,不依赖真实墙钟。

func testReplayWith(t *testing.T, rc *redis.Client, p Publisher, health StoreHealth, cfg Config) *Replay {
	t.Helper()
	cfg.ZoneID, cfg.Batch, cfg.Budget, cfg.Interval = 1, 2, 100*time.Millisecond, 10*time.Millisecond
	r, err := New(rc, rc, p, health, openRouteGate(), cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	r.now = func() time.Time { return time.UnixMilli(fixtureNowMs) }
	r.jitter = func() time.Duration { return 0 }
	return r
}

func resultCount(r *Replay, label string) float64 {
	return testutil.ToFloat64(r.results.WithLabelValues(label))
}

func indexScore(t *testing.T, rc *redis.Client, id uint64) int64 {
	t.Helper()
	score, err := rc.ZScore(context.Background(), pendingIndexKey(1, id%BucketCount), strconv.FormatUint(id, 10)).Result()
	require.NoError(t, err)
	return int64(score)
}

// 正常路径优先:第一次看到某版本只登记、不补投;FirstReplayAfter 之前再到也不投,过了才投第一次。
func TestFirstSightOnlyObservesUntilGrace(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, p.tasks)
	require.Equal(t, in.version, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttemptVersion).Val())
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val())
	require.Equal(t, fixtureNowMs+(30*time.Second).Milliseconds(), indexScore(t, rc, fixtureID))
	require.Equal(t, 1.0, resultCount(r, "observed"))

	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(fixtureNowMs + 29_999) }
	_, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Empty(t, p.tasks, "宽限期内不补投")
	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(fixtureNowMs + 30_000) }
	n, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, "1", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val())
}

// 每次补投后的等待按 RetryAfter 翻倍、封顶 MaxRetryAfter;达到 MaxAttempts 后到期也不再投,
// 按复查间隔推迟并计 exhausted;意图原字节与 pending 不动。
func TestReplayBackoffDoublesAndStopsAtMaxAttempts(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{}
	r := testReplayWith(t, rc, p, nil, Config{RetryAfter: time.Second, MaxRetryAfter: 3 * time.Second, MaxAttempts: 3, RecheckAfter: time.Minute})
	now := fixtureNowMs
	for i, wait := range []int64{1000, 2000, 3000} {
		at := now
		r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(at) }
		n, err := r.Tick(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, n, "第 %d 次补投", i+1)
		require.Equal(t, strconv.Itoa(i+1), rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val())
		require.Equal(t, at+wait, indexScore(t, rc, fixtureID), "第 %d 次之后的等待", i+1)
		now = at + wait
	}
	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(now) }
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "达到上限后不再补投")
	require.Len(t, p.tasks, 6)
	require.Equal(t, now+time.Minute.Milliseconds(), indexScore(t, rc, fixtureID))
	require.Equal(t, 1.0, resultCount(r, "exhausted"))
	require.Equal(t, in.first, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldFirst).Val(), "耗尽不删除意图")
	require.Equal(t, in.version, rc.HGet(ctx, savebarrier.Key(fixtureID), "pending").Val())
}

// 终态停止重投:go/db 对当前版本打了终态之后,补投器不再投它;新版本存盘覆盖意图后终态失效,两表 ACK 齐全时全部字段回收。
func TestTerminalStopsReplayUntilNewerVersion(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	old := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, old)
	marked, err := MarkTerminal(ctx, rc, fixtureID, 7, 8, "blob_too_large")
	require.NoError(t, err)
	require.False(t, marked, "不是当前意图的版本不得打终态")
	marked, err = MarkTerminal(ctx, rc, fixtureID, 7, 9, "blob_too_large")
	require.NoError(t, err)
	require.True(t, marked)

	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, p.tasks, "终态不补投")
	require.Equal(t, 1.0, resultCount(r, "terminal"))
	require.Equal(t, fixtureNowMs+(10*time.Minute).Milliseconds(), indexScore(t, rc, fixtureID))
	require.Equal(t, old.first, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldFirst).Val(), "终态保留原字节")

	newer := testIntent(t, fixtureID, 7, 10, 1)
	storeObservedIntent(t, rc, fixtureID, 1, newer)
	r.bucket = 0
	n, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "新版本不受旧版本终态影响")
	require.Equal(t, newer.first, p.tasks[0].body)
	require.False(t, rc.HExists(ctx, savebarrier.Key(fixtureID), fieldTerminal).Val(), "过期终态被清掉")

	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs)))
	require.NoError(t, rc.HSet(ctx, savebarrier.Key(fixtureID), "player_database", newer.version, "player_database_1", newer.version).Err())
	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(fixtureNowMs + 2_000) }
	_, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), rc.HLen(ctx, savebarrier.Key(fixtureID)).Val(), "两表 ACK 齐全后只剩 pending 与两表 ACK")
	require.Zero(t, rc.ZCard(ctx, pendingIndexKey(1, 0)).Val())
}

// 终态与在途标记只给已有意图的屏障打,绝不凭空创建 save_barrier;在途时刻以最后一次写入为准(r2-db#3,不再取较大值)。
func TestConsumerNotesNeverCreateBarrier(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	marked, err := MarkTerminal(ctx, rc, fixtureID, 7, 9, "stale_owner")
	require.NoError(t, err)
	require.False(t, marked)
	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs)))
	require.Zero(t, rc.Exists(ctx, savebarrier.Key(fixtureID)).Val(), "不得凭空创建屏障")

	committed := savebarrier.Version(7, 9)
	require.NoError(t, rc.HSet(ctx, savebarrier.Key(fixtureID), "pending", committed, "player_database", committed, "player_database_1", committed).Err())
	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs)))
	require.False(t, rc.HExists(ctx, savebarrier.Key(fixtureID), fieldRetryStamp).Val(), "意图已回收时不留在途标记")

	storeIntent(t, rc, fixtureID, 1, testIntent(t, fixtureID, 7, 10, 1))
	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs)))
	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs-5_000)))
	require.Equal(t, strconv.FormatInt(fixtureNowMs-5_000, 10), rc.HGet(ctx, savebarrier.Key(fixtureID), fieldRetryStamp).Val(), "最后一次写入为准")
}

// 本 key 已有在途重试时只推迟、不计次数;窗口过后照常补投。
func TestInflightRetryDefersWithoutConsumingAttempt(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, in)
	require.NoError(t, NoteRetryDeferred(ctx, rc, fixtureID, time.UnixMilli(fixtureNowMs-1_000)))
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1.0, resultCount(r, "inflight_deferred"))
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val(), "推迟不消耗尝试次数")
	require.Equal(t, fixtureNowMs+1_000, indexScore(t, rc, fixtureID))

	r.bucket, r.now = 0, func() time.Time { return time.UnixMilli(fixtureNowMs + (2 * time.Minute).Milliseconds()) }
	n, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "在途窗口过后照常补投")
}

type stubHealth struct {
	cooling bool
	asked   []uint32
}

func (h *stubHealth) CoolingDown(storageID uint32) bool {
	h.asked = append(h.asked, storageID)
	return h.cooling
}

// 本 zone 主库冷却、或本 zone 仍有 legacy 实例在消费时整拍跳过,既不认领也不计次数。
func TestStoreCooldownAndLegacyAliveSkipWholeTick(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	storeObservedIntent(t, rc, fixtureID, 1, testIntent(t, fixtureID, 7, 9, 1))
	health := &stubHealth{cooling: true}
	p := &recordingPublisher{}
	r := testReplayWith(t, rc, p, health, Config{RetryAfter: time.Second})
	n, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, []uint32{1}, health.asked, "只看本 zone 主库")
	require.Equal(t, 1.0, resultCount(r, "store_cooldown"))
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val())

	health.cooling = false
	require.NoError(t, rc.Set(ctx, legacyAliveKey(1), "1", 0).Err())
	r.bucket = 0
	n, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1.0, resultCount(r, "legacy_alive"))
	require.Equal(t, "0", rc.HGet(ctx, savebarrier.Key(fixtureID), fieldAttempts).Val())

	require.NoError(t, rc.Del(ctx, legacyAliveKey(1)).Err())
	r.bucket = 0
	n, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// deploy/k8s/db-task-retry-alerts.yaml 的 db-task.save-outbox 组按这些字符串匹配,且用 increase():序列必须启动即以 0 存在。
func TestReplayResultSeriesPrecreatedForAlerts(t *testing.T) {
	rc := fixtureRedis(t)
	r := testReplay(t, rc, &recordingPublisher{})
	// 先数序列再按 label 读:WithLabelValues 会顺手建出缺失的序列,顺序反了就测不出漏建。
	require.Equal(t, len(replayResults), testutil.CollectAndCount(r.results))
	for _, label := range []string{"exhausted", "terminal", "legacy_alive"} {
		require.Contains(t, replayResults, label)
		require.Zero(t, resultCount(r, label))
	}
}

func TestConfigRejectsUnboundedSchedule(t *testing.T) {
	for name, cfg := range map[string]Config{
		"attempts":        {ZoneID: 1, MaxAttempts: 65},
		"max below base":  {ZoneID: 1, RetryAfter: time.Minute, MaxRetryAfter: time.Second},
		"max too long":    {ZoneID: 1, MaxRetryAfter: 2 * time.Hour},
		"grace too short": {ZoneID: 1, FirstReplayAfter: time.Millisecond},
		"window too long": {ZoneID: 1, InflightWindow: 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cfg.normalized()
			require.Error(t, err)
		})
	}
	cfg, err := Config{ZoneID: 1}.normalized()
	require.NoError(t, err)
	require.Equal(t, 8, cfg.MaxAttempts)
	require.Equal(t, 10*time.Minute, cfg.MaxRetryAfter)
}
