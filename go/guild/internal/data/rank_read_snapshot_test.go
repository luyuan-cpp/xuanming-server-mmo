package data

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 在第一次成功的榜读取响应后，从另一个连接改榜。旧的多命令读取会把前后两版
// 拼在同一回包；单个 Redis 脚本必须返回改榜前的完整快照。
// GUILD_RANK_TEST_REDIS 非空时连真实 Redis；被测的分区榜键在传输边界改为随机前缀，
// 不碰正式 guild_rank:zone:* 键，也不清空实例或数据库。
const snapshotTestZone = uint32(2)

type rankSnapshotHook struct {
	key   string
	after func(context.Context) error
	once  sync.Once
	err   error
}

func (h *rankSnapshotHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *rankSnapshotHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *rankSnapshotHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		// 既覆盖旧的直接命令，也覆盖脚本 KEYS；不修改脚本、成员或返回值。
		for i, arg := range cmd.Args() {
			if key, ok := arg.(string); ok && key == zoneRankKey(snapshotTestZone) {
				cmd.Args()[i] = h.key
			}
		}
		if err := next(ctx, cmd); err != nil {
			return err
		}
		switch cmd.Name() {
		case "zcard", "zrevrank", "eval", "evalsha":
			h.once.Do(func() { h.err = h.after(ctx) })
		}
		return h.err
	}
}

func rankSnapshotFixture(t *testing.T, change func(context.Context, *redis.Client, string) error) *GuildRepo {
	t.Helper()
	var options *redis.Options
	if address := strings.TrimSpace(os.Getenv("GUILD_RANK_TEST_REDIS")); address != "" {
		var err error
		options, err = redis.ParseURL(address)
		require.NoError(t, err)
	} else {
		options = &redis.Options{Addr: miniredis.RunT(t).Addr()}
	}
	options.DialTimeout = 3 * time.Second
	options.ReadTimeout = 3 * time.Second
	options.WriteTimeout = 3 * time.Second
	reader := redis.NewClient(options)
	writer := redis.NewClient(options)
	key := "guild-rank-it:{" + uuid.NewString() + "}:rank"
	t.Cleanup(func() {
		defer reader.Close()
		defer writer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, writer.Del(ctx, key).Err())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.ZAdd(ctx, key, redis.Z{Score: 20, Member: "101"}, redis.Z{Score: 10, Member: "102"}).Err())
	reader.AddHook(&rankSnapshotHook{key: key, after: func(ctx context.Context) error { return change(ctx, writer, key) }})
	return NewGuildRepo(reader, nil, time.Minute)
}

func TestGetGuildRankPageUsesOneSnapshotDuringRemoval(t *testing.T) {
	repo := rankSnapshotFixture(t, func(ctx context.Context, writer *redis.Client, key string) error {
		return writer.ZRem(ctx, key, "101").Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, total, err := repo.GetGuildRankPage(ctx, snapshotTestZone, 1, 10)
	require.NoError(t, err)
	require.Equal(t, uint32(2), total)
	require.Equal(t, []RankEntry{{GuildID: 101, Score: 20, Rank: 1}, {GuildID: 102, Score: 10, Rank: 2}}, entries)
}

func TestGetGuildRankUsesOneSnapshotDuringScoreChange(t *testing.T) {
	repo := rankSnapshotFixture(t, func(ctx context.Context, writer *redis.Client, key string) error {
		return writer.ZAdd(ctx, key, redis.Z{Score: 5, Member: "101"}).Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entry, err := repo.GetGuildRank(ctx, 101, snapshotTestZone)
	require.NoError(t, err)
	require.Equal(t, RankEntry{GuildID: 101, Score: 20, Rank: 1}, entry)
}

func TestGetGuildRankUsesOneSnapshotDuringRemoval(t *testing.T) {
	repo := rankSnapshotFixture(t, func(ctx context.Context, writer *redis.Client, key string) error {
		return writer.ZRem(ctx, key, "101").Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entry, err := repo.GetGuildRank(ctx, 101, snapshotTestZone)
	require.NoError(t, err)
	require.Equal(t, RankEntry{GuildID: 101, Score: 20, Rank: 1}, entry)
	missing, err := repo.GetGuildRank(ctx, 101, snapshotTestZone)
	require.NoError(t, err)
	require.Equal(t, RankEntry{}, missing)
}
