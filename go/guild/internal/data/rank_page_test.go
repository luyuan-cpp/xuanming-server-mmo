package data

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 验证实际 Redis 分页结果:超大页码应为空,不能因乘法溢出重新读到前几名。
// 不依赖 MySQL;同时覆盖客户端受限页长与内部调用的完整 uint32 参数范围。
func TestGetGuildRankPageHandlesPaginationBoundaries(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := NewGuildRepo(rdb, nil, time.Minute)
	ctx := context.Background()
	members := make([]redis.Z, 6)
	for i := range members {
		members[i] = redis.Z{Score: float64(60 - i*10), Member: strconv.Itoa(101 + i)}
	}
	for _, key := range []string{guildRankKey, zoneRankKey(2)} {
		require.NoError(t, rdb.ZAdd(ctx, key, members...).Err())
	}

	const maxUint32 = 1<<32 - 1
	cases := []struct {
		name          string
		page          uint32
		pageSize      uint32
		wantGuildIDs  []uint64
		wantFirstRank uint32
	}{
		{name: "首页", page: 1, pageSize: 4, wantGuildIDs: []uint64{101, 102, 103, 104}, wantFirstRank: 1},
		{name: "不满一页的末页", page: 2, pageSize: 4, wantGuildIDs: []uint64{105, 106}, wantFirstRank: 5},
		{name: "超过末页", page: 3, pageSize: 4},
		{name: "页码为零", page: 0, pageSize: 4},
		{name: "页长为零", page: 1, pageSize: 0},
		// 旧算法用 uint32 相乘,结果环绕为 4,错误返回第五、六名。
		{name: "超过uint32的起点", page: 214748366, pageSize: 20},
		// 直接改为 int64 相乘仍会溢出;完整乘积必须先按无符号值判定空页。
		{name: "超过int64的起点", page: maxUint32, pageSize: maxUint32},
		{name: "内部调用最大页长", page: 1, pageSize: maxUint32,
			wantGuildIDs: []uint64{101, 102, 103, 104, 105, 106}, wantFirstRank: 1},
	}
	for _, zoneID := range []uint32{0, 2} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("zone=%d/%s", zoneID, tc.name), func(t *testing.T) {
				entries, total, err := repo.GetGuildRankPage(ctx, zoneID, tc.page, tc.pageSize)
				require.NoError(t, err)
				assert.Equal(t, uint32(6), total)
				require.Len(t, entries, len(tc.wantGuildIDs))
				for i, guildID := range tc.wantGuildIDs {
					assert.Equal(t, guildID, entries[i].GuildID)
					assert.Equal(t, tc.wantFirstRank+uint32(i), entries[i].Rank)
				}
			})
		}
	}
}
