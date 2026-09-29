package routing

import (
	"context"
	"strconv"
	"testing"

	"data_service/internal/config"

	"shared/placement"

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

	err := r.RegisterPlayerZone(ctx, 1001, 5, 0)
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

func TestRegisterPlayerZone_RefusedWhileZoneIsMerging(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	// 合服工具立标记:键存在即封锁,值只是给人看的。
	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))

	err := r.RegisterPlayerZone(ctx, 7003, 12, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrZoneMergeInProgress)

	// 零变更:被闸门拒绝的登记不许留下半条映射。
	_, err = r.GetPlayerHomeZone(ctx, 7003)
	assert.ErrorIs(t, err, ErrHomeZoneNotMapped)

	// 别的 zone 不受影响 —— 闸门是按 zone 的,不是全局的。
	require.NoError(t, r.RegisterPlayerZone(ctx, 7004, 13, 0))

	// 标记清除后恢复放行(工具跑完 DEL,或 TTL 到期)。
	mr.Del(MergeFenceKey(12))
	require.NoError(t, r.RegisterPlayerZone(ctx, 7003, 12, 0))
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

// ── 落点记录(player-storage-placement.md §8.1)────────────────────

// TestKeyContractsMatchSharedPlacement:本包自己拼的 player:zone / merge:in_progress 键
// 必须与 shared/placement 逐字节相同 —— go/db 与搬库工具按那一份读,两边一旦分叉,
// 合服围栏与落点选库就各看各的键,且全程零报错。
func TestKeyContractsMatchSharedPlacement(t *testing.T) {
	for _, pid := range []uint64{1, 7001, 18446744073709551615} {
		assert.Equal(t, placement.HomeZoneKey(pid), mappingKey(pid))
	}
	for _, zone := range []uint32{1, 42, 4294967295} {
		assert.Equal(t, placement.MergeFenceKey(zone), MergeFenceKey(zone))
		// getHomeZoneAndMergeFenceScript 在 Lua 里用「前缀 .. home 原始值」拼围栏键。
		assert.Equal(t, placement.MergeFenceKey(zone), mergeFenceKeyPrefix+strconv.FormatUint(uint64(zone), 10))
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

// TestRegisterPlayerZone_FencedWritesNeitherKey:合服围栏拒绝时,带 storage_id 也一样零变更。
func TestRegisterPlayerZone_FencedWritesNeitherKey(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()
	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))

	err := r.RegisterPlayerZone(ctx, 8005, 12, placement.DefaultGlobalStorageID)
	require.ErrorIs(t, err, ErrZoneMergeInProgress)
	assert.False(t, mr.Exists(mappingKey(8005)))
	assert.False(t, mr.Exists(placement.Key(8005)))
}

// TestRegisterPlayerZone_FenceRecheckedAtCommitPoint:预检放行之后、写入脚本执行之前
// merge_zone 立起围栏(先立围栏再收集清单),提交点的脚本复核必须拒绝,两键零变更 ——
// 否则这名玩家不在合服清单里,映射合服后仍指向源区。
// 直接调用提交点 registerPlayerZoneFenced 来模拟「预检之后才立围栏」的时序。
func TestRegisterPlayerZone_FenceRecheckedAtCommitPoint(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()
	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))

	for _, tc := range []struct {
		name      string
		playerID  uint64
		storageID uint32
	}{
		{"with storage_id", 8007, placement.DefaultGlobalStorageID},
		{"without storage_id", 8008, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.registerPlayerZoneFenced(ctx, tc.playerID, 12, tc.storageID)
			require.ErrorIs(t, err, ErrZoneMergeInProgress)
			assert.False(t, mr.Exists(mappingKey(tc.playerID)), "围栏内不得写 player:zone")
			assert.False(t, mr.Exists(placement.Key(tc.playerID)), "围栏内不得写 player:placement")
		})
	}

	// 围栏只拦它自己的 zone:别的 zone 照常登记。
	require.NoError(t, r.registerPlayerZoneFenced(ctx, 8009, 13, placement.DefaultGlobalStorageID))
	home, err := mr.Get(mappingKey(8009))
	require.NoError(t, err)
	assert.Equal(t, "13", home)
	assert.True(t, mr.Exists(placement.Key(8009)))

	// 撤掉围栏后同一玩家可以登记,且围栏期间的拒绝没有留下任何半截状态。
	mr.Del(MergeFenceKey(12))
	require.NoError(t, r.registerPlayerZoneFenced(ctx, 8007, 12, placement.DefaultGlobalStorageID))
	raw, err := mr.Get(placement.Key(8007))
	require.NoError(t, err)
	assert.Equal(t, placement.StableValue(placement.DefaultGlobalStorageID, 1), raw)
}

// TestRegisterPlayerZone_FenceCheckPrecedesConflict:围栏内即使 home 已存在(同值或异值),
// 也回 ErrZoneMergeInProgress,不回冲突/幂等成功,且既有映射不动。
func TestRegisterPlayerZone_FenceCheckPrecedesConflict(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()
	require.NoError(t, r.RegisterPlayerZone(ctx, 8010, 12, 0))
	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))

	require.ErrorIs(t, r.registerPlayerZoneFenced(ctx, 8010, 12, 0), ErrZoneMergeInProgress)
	home, err := mr.Get(mappingKey(8010))
	require.NoError(t, err)
	assert.Equal(t, "12", home)
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

// ── GetPlayerHomeZoneAndMergeFence(A16)──────────────────────────

func TestGetPlayerHomeZoneAndMergeFence(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()

	_, _, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 8101)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "缺席的口径必须与 GetPlayerHomeZone 相同")

	require.NoError(t, r.RegisterPlayerZone(ctx, 8101, 12, 0))
	zone, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 8101)
	require.NoError(t, err)
	assert.Equal(t, uint32(12), zone)
	assert.False(t, merging)

	// 别的 zone 的围栏不影响本玩家:围栏是按 home 查的。
	require.NoError(t, mr.Set(MergeFenceKey(13), `{"started_at":1757000000}`))
	_, merging, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 8101)
	require.NoError(t, err)
	assert.False(t, merging)

	require.NoError(t, mr.Set(MergeFenceKey(12), `{"started_at":1757000000}`))
	zone, merging, err = r.GetPlayerHomeZoneAndMergeFence(ctx, 8101)
	require.NoError(t, err)
	assert.Equal(t, uint32(12), zone, "合服中也要照常返回 home,调用方据 merging 拒绝")
	assert.True(t, merging)
}

// TestGetPlayerHomeZoneAndMergeFence_RedisFailureIsError:脚本整体失败(拿不到 home)时是
// 普通错误,不能伪装成「没有映射」。
func TestGetPlayerHomeZoneAndMergeFence_RedisFailureIsError(t *testing.T) {
	r, mr := newTestRouter(t)
	ctx := context.Background()
	require.NoError(t, r.RegisterPlayerZone(ctx, 8102, 12, 0))

	mr.Close()
	_, _, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 8102)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped)
}

// TestDecodeHomeZoneAndMergeFenceReply 覆盖脚本回复的每一种形态。「围栏读失败」只能在这里测:
// miniredis 没法让脚本里单条 EXISTS 失败(真 Redis 上它会在 Cluster 跨 slot / ACL 拒绝时发生)。
func TestDecodeHomeZoneAndMergeFenceReply(t *testing.T) {
	zone, merging, err := decodeHomeZoneAndMergeFenceReply(1, []any{"12", int64(0)})
	require.NoError(t, err)
	assert.Equal(t, uint32(12), zone)
	assert.False(t, merging)

	zone, merging, err = decodeHomeZoneAndMergeFenceReply(1, []any{"12", int64(1)})
	require.NoError(t, err)
	assert.Equal(t, uint32(12), zone)
	assert.True(t, merging)

	// 围栏读失败 → fail-closed:当作正在合服,home 照常返回。
	zone, merging, err = decodeHomeZoneAndMergeFenceReply(1, []any{"12", mergeFenceUnreadable})
	require.NoError(t, err)
	assert.Equal(t, uint32(12), zone)
	assert.True(t, merging, "围栏读失败必须按正在合服处理")

	// 意料之外的围栏值同样当封锁。
	_, merging, err = decodeHomeZoneAndMergeFenceReply(1, []any{"12", int64(7)})
	require.NoError(t, err)
	assert.True(t, merging)

	_, _, err = decodeHomeZoneAndMergeFenceReply(1, []any{"", int64(0)})
	assert.ErrorIs(t, err, ErrHomeZoneNotMapped)

	_, _, err = decodeHomeZoneAndMergeFenceReply(1, []any{"abc", int64(0)})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped, "畸形值是故障,不是缺席")

	for _, bad := range []any{nil, int64(1), []any{"12"}, []any{int64(12), int64(0)}, []any{"12", "0"}} {
		_, _, err = decodeHomeZoneAndMergeFenceReply(1, bad)
		assert.Error(t, err, "reply=%v", bad)
	}
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

func TestDeletePlayerZone(t *testing.T) {
	r, _ := newTestRouter(t)
	ctx := context.Background()

	_ = r.RegisterPlayerZone(ctx, 500, 7, 0)
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

	_ = r.RegisterPlayerZone(ctx, 42, 1, 0)
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

	_ = r.RegisterPlayerZone(ctx, 101, 10, 0)
	_ = r.RegisterPlayerZone(ctx, 102, 10, 0)
	_ = r.RegisterPlayerZone(ctx, 103, 20, 0)
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

	_ = r.RegisterPlayerZone(ctx, 201, 7, 0)
	_ = r.RegisterPlayerZone(ctx, 202, 8, 0)
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

	_ = r.RegisterPlayerZone(ctx, 301, 5, 0)

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
