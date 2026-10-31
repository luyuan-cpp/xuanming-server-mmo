package routing

import (
	"context"
	"testing"
	"time"

	"data_service/internal/config"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

func newTestRouter(t *testing.T) (*Router, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)

	c := config.Config{
		MappingRedis: redis.RedisConf{
			Host: mr.Addr(),
			Type: "node",
		},
	}
	r := NewRouter(c)
	t.Cleanup(func() { r.Close() })
	return r, mr
}

func TestRegisterAndGetPlayerZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	err := r.RegisterPlayerZone(ctx, 1001, 5, 0)
	assert.NoError(t, err)

	zone, err := r.GetPlayerHomeZone(ctx, 1001)
	assert.NoError(t, err)
	assert.Equal(t, uint32(5), zone)
}

// ── SETNX 语义 ───────────────────────────────────────

// TestRegisterPlayerZone_NeverOverwritesDifferentZone 是本次修复的核心:
// 已登记的归属不许被任何"按建角 zone 重新登记"的路径改写。
func TestRegisterPlayerZone_NeverOverwritesDifferentZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 7001, 3, 0))

	err := r.RegisterPlayerZone(ctx, 7001, 9, 0)
	require.Error(t, err)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict, "冲突必须是可判定的类型,调用方才能翻成专属错误码")
	assert.Equal(t, uint64(7001), conflict.PlayerID)
	assert.Equal(t, uint32(3), conflict.Existing, "错误里必须带上既有 zone")
	assert.Equal(t, uint32(9), conflict.Requested)
	assert.Contains(t, conflict.Error(), "already mapped to home zone 3")

	// 拒绝必须是零变更
	zone, err := r.GetPlayerHomeZone(ctx, 7001)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), zone)
}

// TestRegisterPlayerZone_SameZoneIsIdempotent:login 的 CreatePlayer 允许重试,
// 重复登记同一个 zone 不能变成失败。
func TestRegisterPlayerZone_SameZoneIsIdempotent(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 7002, 4, 0))
	require.NoError(t, r.RegisterPlayerZone(ctx, 7002, 4, 0))

	zone, err := r.GetPlayerHomeZone(ctx, 7002)
	require.NoError(t, err)
	assert.Equal(t, uint32(4), zone)
}

// ── 落点记录(player-storage-placement.md §8.1)────────────────────

// TestKeyContractsMatchSharedPlacement:本包自己拼的 player:zone 键
// 必须与 shared/placement 逐字节相同 —— go/db 与搬库工具按那一份读,两边一旦分叉,
// 落点选库就看错键,且全程零报错。
func TestKeyContractsMatchSharedPlacement(t *testing.T) {
	for _, pid := range []uint64{1, 7001, 18446744073709551615} {
		assert.Equal(t, placement.HomeZoneKey(pid), mappingKey(pid))
	}
}

// TestRegisterPlayerZone_WithStorageIDPinsPlacement:storage_id 非 0 时,映射与稳定态落点
// "{storage_id}:1" 一起写下。
func TestRegisterPlayerZone_WithStorageIDPinsPlacement(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 8001, 5, placement.DefaultGlobalStorageID))

	home, err := mr.Get(mappingKey(8001))
	require.NoError(t, err)
	assert.Equal(t, "5", home)
	raw, err := mr.Get(placement.Key(8001))
	require.NoError(t, err)
	assert.Equal(t, placement.StableValue(placement.DefaultGlobalStorageID, 1), raw)
	rec, err := placement.Parse(raw)
	require.NoError(t, err, "写下的值必须能被 go/db 用同一份契约解析")
	assert.Equal(t, placement.Record{StorageID: placement.DefaultGlobalStorageID, Version: 1}, rec)
}

// TestRegisterPlayerZone_WithoutStorageIDWritesNoPlacement:storage_id=0 是旧行为,
// 不许顺手留下落点记录(有效落点应回落 home_zone)。
func TestRegisterPlayerZone_WithoutStorageIDWritesNoPlacement(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 8002, 5, 0))
	assert.True(t, mr.Exists(mappingKey(8002)))
	assert.False(t, mr.Exists(placement.Key(8002)))
}

// TestRegisterPlayerZone_ExistingHomeLeavesPlacementUntouched 是原子性的核心:home 已存在时,
// 无论同值幂等还是冲突,落点键都不许被写 —— 否则一次带 storage_id 的重试或误登记
// 就能把玩家的数据指针挪走。
func TestRegisterPlayerZone_ExistingHomeLeavesPlacementUntouched(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	// 旧号(登记时没钉落点)被带 storage_id 的请求重登:幂等成功,落点仍缺席。
	require.NoError(t, r.RegisterPlayerZone(ctx, 8003, 5, 0))
	require.NoError(t, r.RegisterPlayerZone(ctx, 8003, 5, placement.DefaultGlobalStorageID))
	assert.False(t, mr.Exists(placement.Key(8003)), "home 已存在时同值重试不得补写落点")

	// 冲突:拒绝且两键零变更。
	err := r.RegisterPlayerZone(ctx, 8003, 9, placement.DefaultGlobalStorageID)
	var conflict *HomeZoneConflictError
	require.ErrorAs(t, err, &conflict)
	assert.False(t, mr.Exists(placement.Key(8003)), "冲突的登记不得留下落点")
	home, err := mr.Get(mappingKey(8003))
	require.NoError(t, err)
	assert.Equal(t, "5", home)

	// 已钉过落点的号用另一个 storage_id 重试:幂等成功,原落点原样保留。
	require.NoError(t, r.RegisterPlayerZone(ctx, 8004, 5, 102))
	require.NoError(t, r.RegisterPlayerZone(ctx, 8004, 5, placement.DefaultGlobalStorageID))
	raw, err := mr.Get(placement.Key(8004))
	require.NoError(t, err)
	assert.Equal(t, "102:1", raw)
}

// TestRegisterPlayerZone_KeepsPreexistingPlacement:home 缺席但落点记录已在(映射曾被删),
// home 照常写入,落点不被请求里的 storage_id 覆盖 —— 那条记录指向数据真正所在的库。
func TestRegisterPlayerZone_KeepsPreexistingPlacement(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()
	require.NoError(t, mr.Set(placement.Key(8006), "7:3"))

	require.NoError(t, r.RegisterPlayerZone(ctx, 8006, 5, placement.DefaultGlobalStorageID))
	home, err := mr.Get(mappingKey(8006))
	require.NoError(t, err)
	assert.Equal(t, "5", home)
	raw, err := mr.Get(placement.Key(8006))
	require.NoError(t, err)
	assert.Equal(t, "7:3", raw)
}

func TestGetPlayerHomeZone_NotFound(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_, err := r.GetPlayerHomeZone(ctx, 9999)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no home zone mapping")
}

func TestBatchGetPlayerHomeZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 1, 10, 0)
	_ = r.RegisterPlayerZone(ctx, 2, 20, 0)
	_ = r.RegisterPlayerZone(ctx, 3, 30, 0)

	result, err := r.BatchGetPlayerHomeZone(ctx, []uint64{1, 2, 3, 999})
	assert.NoError(t, err)
	assert.Equal(t, uint32(10), result[1])
	assert.Equal(t, uint32(20), result[2])
	assert.Equal(t, uint32(30), result[3])
	_, exists := result[999]
	assert.False(t, exists)
}

// 畸形值不再被当成缺席静默跳过:整批报错,与单查 GetPlayerHomeZone 同口径。
func TestBatchGetPlayerHomeZone_MalformedValueFailsTheBatch(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 1, 10, 0)
	require.NoError(t, mr.Set(mappingKey(2), "not-a-zone"))

	result, err := r.BatchGetPlayerHomeZone(ctx, []uint64{1, 2, 999})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid zone mapping for player 2")

	_, singleErr := r.GetPlayerHomeZone(ctx, 2)
	require.Error(t, singleErr)
	assert.NotErrorIs(t, singleErr, ErrHomeZoneNotMapped, "畸形值不是缺席")
}

// cancelOnSetNXClient 在数据侧 SETNX 之前取消请求 ctx,模拟「mapping 锁已拿到、第二把锁之前请求超时」。
type cancelOnSetNXClient struct {
	*goredis.Client
	cancel context.CancelFunc
}

func (c *cancelOnSetNXClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.BoolCmd {
	c.cancel()
	return c.Client.SetNX(ctx, key, value, expiration)
}
