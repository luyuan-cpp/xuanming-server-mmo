package routing

import (
	"context"
	"testing"

	"data_service/internal/config"

	"github.com/alicebob/miniredis/v2"
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
		DevRedis: config.DevRedisConfig{
			Host: mr.Addr(),
			DB:   0,
		},
		PlayerLockTTLSec: 3,
	}
	r := NewRouter(c)
	t.Cleanup(func() { r.Close() })
	return r, mr
}

func TestRegisterAndGetPlayerZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	err := r.RegisterPlayerZone(ctx, 1001, 5)
	assert.NoError(t, err)

	zone, err := r.GetPlayerHomeZone(ctx, 1001)
	assert.NoError(t, err)
	assert.Equal(t, uint32(5), zone)
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

	_ = r.RegisterPlayerZone(ctx, 1, 10)
	_ = r.RegisterPlayerZone(ctx, 2, 20)
	_ = r.RegisterPlayerZone(ctx, 3, 30)

	result, err := r.BatchGetPlayerHomeZone(ctx, []uint64{1, 2, 3, 999})
	assert.NoError(t, err)
	assert.Equal(t, uint32(10), result[1])
	assert.Equal(t, uint32(20), result[2])
	assert.Equal(t, uint32(30), result[3])
	_, exists := result[999]
	assert.False(t, exists)
}

func TestDeletePlayerZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 500, 7)
	zone, err := r.GetPlayerHomeZone(ctx, 500)
	assert.NoError(t, err)
	assert.Equal(t, uint32(7), zone)

	err = r.DeletePlayerZone(ctx, 500)
	assert.NoError(t, err)

	_, err = r.GetPlayerHomeZone(ctx, 500)
	assert.Error(t, err)
}

func TestClientForPlayer_DevMode(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 42, 1)
	client, err := r.ClientForPlayer(ctx, 42)
	assert.NoError(t, err)
	assert.NotNil(t, client)
}

func TestClientForZone_DevMode(t *testing.T) {
	r, _ := newTestRouter(t)

	client, err := r.ClientForZone(999)
	assert.NoError(t, err)
	assert.NotNil(t, client) // dev mode returns devClient for any zone
}

func TestRemapHomeZoneForMerge_DryRun(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 101, 10)
	_ = r.RegisterPlayerZone(ctx, 102, 10)
	_ = r.RegisterPlayerZone(ctx, 103, 20)

	matched, updated, err := r.RemapHomeZoneForMerge(ctx, 10, 99, true)
	assert.NoError(t, err)
	assert.Equal(t, 2, matched)
	assert.Equal(t, 0, updated)

	z101, _ := r.GetPlayerHomeZone(ctx, 101)
	assert.Equal(t, uint32(10), z101)
}

func TestRemapHomeZoneForMerge_Apply(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 201, 7)
	_ = r.RegisterPlayerZone(ctx, 202, 8)

	matched, updated, err := r.RemapHomeZoneForMerge(ctx, 7, 11, false)
	assert.NoError(t, err)
	assert.Equal(t, 1, matched)
	assert.Equal(t, 1, updated)

	z201, _ := r.GetPlayerHomeZone(ctx, 201)
	assert.Equal(t, uint32(11), z201)
	z202, _ := r.GetPlayerHomeZone(ctx, 202)
	assert.Equal(t, uint32(8), z202)
}

func TestAcquireAndReleasePlayerLock(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()
	client, err := r.ClientForZone(1)
	require.NoError(t, err)

	token, ok, err := r.AcquirePlayerLock(ctx, client, 123)
	assert.NoError(t, err)
	assert.True(t, ok)
	assert.NotEmpty(t, token)

	// Second acquire should fail
	token2, ok2, err := r.AcquirePlayerLock(ctx, client, 123)
	assert.NoError(t, err)
	assert.False(t, ok2)
	assert.Empty(t, token2)

	// Release and re-acquire should succeed
	err = r.ReleasePlayerLock(ctx, client, 123, token)
	assert.NoError(t, err)

	token3, ok3, err := r.AcquirePlayerLock(ctx, client, 123)
	assert.NoError(t, err)
	assert.True(t, ok3)
	assert.NotEmpty(t, token3)
}

// TestReleasePlayerLock_WrongTokenDoesNotUnlock 覆盖修复本身:
// 前任持锁者(锁已过期/被顶替)的释放不得删掉现任的锁。
func TestReleasePlayerLock_WrongTokenDoesNotUnlock(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()
	client, err := r.ClientForZone(1)
	require.NoError(t, err)

	staleToken := "not-the-owner"
	token, ok, err := r.AcquirePlayerLock(ctx, client, 456)
	assert.NoError(t, err)
	assert.True(t, ok)

	// 拿着错 token 释放:不报错,但锁必须还在
	err = r.ReleasePlayerLock(ctx, client, 456, staleToken)
	assert.NoError(t, err)

	_, ok2, err := r.AcquirePlayerLock(ctx, client, 456)
	assert.NoError(t, err)
	assert.False(t, ok2, "lock must survive a release attempt with a stale token")

	// 正主释放后才可再次获取
	assert.NoError(t, r.ReleasePlayerLock(ctx, client, 456, token))
	_, ok3, err := r.AcquirePlayerLock(ctx, client, 456)
	assert.NoError(t, err)
	assert.True(t, ok3)
}
