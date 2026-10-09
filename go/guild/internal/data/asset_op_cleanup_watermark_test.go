package data

// 清理水位的用例(07-rollback-fail-closed.md §7.4.2 落码修正)。前两条只用 miniredis、不连库;
// 第三条经真实 CleanupOnce 钉住"先推水位再删、水位写不进就不删",需要 GUILD_TEST_MYSQL_DSN(未设则跳过)。

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/guild"
)

func newWatermarkOnlyStore(t *testing.T) (*GuildAssetStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return &GuildAssetStore{guilds: &GuildRepo{rdb: rdb}}, mr
}

// 水位只增不减:多副本各自跑清理、墙钟略有先后,较旧的截止不能把水位拉回去。
func TestTerminalCleanupWatermark_OnlyAdvances(t *testing.T) {
	store, mr := newWatermarkOnlyStore(t)
	ctx := context.Background()

	got, err := store.TerminalCleanupWatermarkMs(ctx)
	require.NoError(t, err)
	assert.Zero(t, got, "键不存在 = 从未清理过")

	const first, older, newer = uint64(1_700_000_000_000), uint64(1_699_999_999_000), uint64(1_700_000_600_000)
	require.NoError(t, store.advanceTerminalCleanupWatermark(ctx, first))
	require.NoError(t, store.advanceTerminalCleanupWatermark(ctx, older))
	got, err = store.TerminalCleanupWatermarkMs(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, got, "较旧的截止不回退水位")

	require.NoError(t, store.advanceTerminalCleanupWatermark(ctx, newer))
	got, err = store.TerminalCleanupWatermarkMs(ctx)
	require.NoError(t, err)
	assert.Equal(t, newer, got)

	raw, err := mr.Get(terminalCleanupWatermarkKey)
	require.NoError(t, err)
	assert.Equal(t, "1700000600000", raw, "落盘是十进制毫秒,不是科学计数法")
	assert.Zero(t, mr.TTL(terminalCleanupWatermarkKey), "水位不设过期")
}

// 读不准一律报错(调用方 fail-closed):Redis 出错、值损坏、没有 Redis 客户端,都不能当成"从未清理过"。
func TestTerminalCleanupWatermark_UnreadableIsAnError(t *testing.T) {
	store, mr := newWatermarkOnlyStore(t)
	ctx := context.Background()

	require.NoError(t, mr.Set(terminalCleanupWatermarkKey, "1.7e+12"))
	_, err := store.TerminalCleanupWatermarkMs(ctx)
	require.Error(t, err, "值损坏")

	mr.SetError("ERR simulated redis failure")
	_, err = store.TerminalCleanupWatermarkMs(ctx)
	require.Error(t, err, "Redis 出错")
	require.Error(t, store.advanceTerminalCleanupWatermark(ctx, 1), "写不进要让调用方知道")
	mr.SetError("")

	noRedis := &GuildAssetStore{guilds: &GuildRepo{}}
	_, err = noRedis.TerminalCleanupWatermarkMs(ctx)
	require.ErrorIs(t, err, errNoWatermarkRedis)
	require.ErrorIs(t, noRedis.advanceTerminalCleanupWatermark(ctx, 1), errNoWatermarkRedis)
}

// CleanupOnce:水位先于删除推进(write-ahead);水位写不进时终态行一行都不删、错误如实返回;
// Redis 恢复后下一轮照常删,水位等于本轮截止。
func TestCleanupOnce_AdvancesWatermarkBeforeDeletingTerminalRows(t *testing.T) {
	f := openEconomyFixture(t)
	const player = uint64(4400)
	conf := CleanupConf{Interval: time.Hour, TerminalRetention: 30 * 24 * time.Hour, CounterRetention: 40 * 24 * time.Hour}
	now := time.UnixMilli(int64(testNowMs))
	cutoff := testNowMs - uint64(conf.TerminalRetention/time.Millisecond)
	oldMs := cutoff - econDayMs

	rec := econShopRecord(t, 7701, player, oldMs, 1)
	rec.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	rec.NextAttemptMs = oldMs
	rec.CreatedMs, rec.UpdatedMs = oldMs, oldMs
	econInsertOp(t, f, rec)
	countRow := func() int {
		var n int
		require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM guild_asset_op WHERE op_id = ?", rec.GetOpId()).Scan(&n))
		return n
	}

	// 水位写不进:换成一个连不上的 Redis 客户端。
	healthy := f.repo.rdb
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { broken.Close() })
	f.repo.rdb = broken
	err := f.store.CleanupOnce(f.ctx, now, conf)
	require.Error(t, err, "水位写不进必须报出来")
	assert.Equal(t, 1, countRow(), "水位没记下就不许删终态行")

	f.repo.rdb = healthy
	require.NoError(t, f.store.CleanupOnce(f.ctx, now, conf))
	assert.Zero(t, countRow(), "保留期外的 APPLIED 行被删")
	got, err := f.store.TerminalCleanupWatermarkMs(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, cutoff, got, "水位 = 本轮清理的截止时刻")
}
