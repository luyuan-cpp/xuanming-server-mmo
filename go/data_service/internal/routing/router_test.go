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

// ── SETNX 语义 + 合服闸门 ───────────────────────────────────────

// TestRegisterPlayerZone_NeverOverwritesDifferentZone 是本次修复的核心:
// 合服把映射改到目标 zone 之后,任何"按建角 zone 重新登记"的路径都不许把它写回去。
func TestRegisterPlayerZone_NeverOverwritesDifferentZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	require.NoError(t, r.RegisterPlayerZone(ctx, 7001, 3))

	err := r.RegisterPlayerZone(ctx, 7001, 9)
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

	require.NoError(t, r.RegisterPlayerZone(ctx, 7002, 4))
	require.NoError(t, r.RegisterPlayerZone(ctx, 7002, 4))

	zone, err := r.GetPlayerHomeZone(ctx, 7002)
	require.NoError(t, err)
	assert.Equal(t, uint32(4), zone)
}

func TestRegisterPlayerZone_RefusedWhileZoneIsMerging(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	// 合服工具立标记:键存在即封锁,值只是给人看的。
	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))

	err := r.RegisterPlayerZone(ctx, 7003, 12)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrZoneMergeInProgress)

	// 零变更:被闸门拒绝的登记不许留下半条映射。
	_, err = r.GetPlayerHomeZone(ctx, 7003)
	assert.ErrorIs(t, err, ErrHomeZoneNotMapped)

	// 别的 zone 不受影响 —— 闸门是按 zone 的,不是全局的。
	require.NoError(t, r.RegisterPlayerZone(ctx, 7004, 13))

	// 标记清除后恢复放行(工具跑完 DEL,或 TTL 到期)。
	mr.Del(MergeFenceKey(12))
	require.NoError(t, r.RegisterPlayerZone(ctx, 7003, 12))
}

func TestIsMergeInProgress(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	assert.Equal(t, "merge:in_progress:42", MergeFenceKey(42), "键名是与合服工具共享的契约")

	fenced, err := r.IsMergeInProgress(ctx, 42)
	require.NoError(t, err)
	assert.False(t, fenced)

	require.NoError(t, mr.Set(MergeFenceKey(42), `{"started_at":1757000000}`))
	fenced, err = r.IsMergeInProgress(ctx, 42)
	require.NoError(t, err)
	assert.True(t, fenced)
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
	r, mr := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 101, 10)
	_ = r.RegisterPlayerZone(ctx, 102, 10)
	_ = r.RegisterPlayerZone(ctx, 103, 20)
	// 闸门先立起来:remap(含 dry-run)要求源 zone 已被封锁,见下面的
	// TestRemapHomeZoneForMerge_RefusedWithoutFence。
	require.NoError(t, mr.Set(MergeFenceKey(10), `{"started_at":1757000000}`))

	matched, updated, err := r.RemapHomeZoneForMerge(ctx, 10, 99, true)
	assert.NoError(t, err)
	assert.Equal(t, 2, matched)
	assert.Equal(t, 0, updated)

	z101, _ := r.GetPlayerHomeZone(ctx, 101)
	assert.Equal(t, uint32(10), z101)
}

func TestRemapHomeZoneForMerge_Apply(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 201, 7)
	_ = r.RegisterPlayerZone(ctx, 202, 8)
	require.NoError(t, mr.Set(MergeFenceKey(7), `{"started_at":1757000000}`))

	matched, updated, err := r.RemapHomeZoneForMerge(ctx, 7, 11, false)
	assert.NoError(t, err)
	assert.Equal(t, 1, matched)
	assert.Equal(t, 1, updated)

	z201, _ := r.GetPlayerHomeZone(ctx, 201)
	assert.Equal(t, uint32(11), z201)
	z202, _ := r.GetPlayerHomeZone(ctx, 202)
	assert.Equal(t, uint32(8), z202)
}

// TestRemapHomeZoneForMerge_RefusedWithoutFence:没立标记就改写 = 源 zone 仍在
// 接受新映射,SCAN 游标之后写进来的玩家会漏网。必须拒绝且零变更。
func TestRemapHomeZoneForMerge_RefusedWithoutFence(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 301, 5)

	for _, dryRun := range []bool{true, false} {
		matched, updated, err := r.RemapHomeZoneForMerge(ctx, 5, 6, dryRun)
		require.Error(t, err, "dry_run=%t", dryRun)
		assert.ErrorIs(t, err, ErrMergeFenceMissing)
		assert.Contains(t, err.Error(), "merge:in_progress:5", "错误要告诉运维缺哪把键")
		assert.Zero(t, matched)
		assert.Zero(t, updated)
	}

	z301, err := r.GetPlayerHomeZone(ctx, 301)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), z301, "被拒绝的 remap 必须零变更")
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
