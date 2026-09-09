package server

import (
	"context"
	"net"
	"testing"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/routing"
	"data_service/internal/svc"
	"proto/data_service"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// newHomeZoneTestServer 只装 Router(mapping Redis 指向 miniredis),其余 store
// 保持 nil —— GetPlayerHomeZone 只碰 mapping,不需要 MySQL。
func newHomeZoneTestServer(t *testing.T) (*DataServiceServer, *miniredis.Miniredis) {
	t.Helper()
	return newHomeZoneTestServerWithConfig(t, func(*config.Config) {})
}

// newHomeZoneTestServerWithConfig 同上,但允许改配置(目前只用来配 AdminToken)。
func newHomeZoneTestServerWithConfig(t *testing.T, tweak func(*config.Config)) (*DataServiceServer, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis:     redis.RedisConf{Host: mr.Addr(), Type: "node"},
		DevRedis:         config.DevRedisConfig{Host: mr.Addr()},
		PlayerLockTTLSec: 3,
	}
	tweak(&c)
	r := routing.NewRouter(c)
	t.Cleanup(r.Close)
	return NewDataServiceServer(&svc.ServiceContext{Config: c, Router: r}), mr
}

// ── GetPlayerHomeZone:缺失 ≠ 故障 ──────────────────────────────

// TestGetPlayerHomeZone_MissingMappingIsNotFound 钉住本次修复:
// 「这名玩家没有映射」必须是一个**专门的码**(NotFound),而不是与传输故障
// 混在一起的 (0, nil) 或 Unknown。login 侧 homezone.IsUnmapped 认 NotFound,
// 并把它归一成"回退到建角 zone",同时把 Unavailable 当故障告警。
func TestGetPlayerHomeZone_MissingMappingIsNotFound(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)

	resp, err := s.GetPlayerHomeZone(context.Background(), &data_service.GetPlayerHomeZoneRequest{PlayerId: 424242})
	require.Error(t, err)
	assert.Nil(t, resp)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
	// 文案是**契约**:滚动升级期间 login 靠它认出老版本 data_service 的 Unknown 响应。
	assert.Contains(t, st.Message(), "no home zone mapping")
	assert.Contains(t, st.Message(), "424242")
}

func TestGetPlayerHomeZone_ReturnsRegisteredZone(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1001, HomeZoneId: 7})
	require.NoError(t, err)

	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1001})
	require.NoError(t, err)
	assert.Equal(t, uint32(7), resp.GetHomeZoneId())
}

// TestGetPlayerHomeZone_RedisFailureIsUnavailableNotNotFound:传输层故障与
// 「没有映射」必须是两个不同的码,否则一次 Redis 宕机会伪装成一批"老玩家",
// 合服当天最需要这条信号时它恰好是哑的。
func TestGetPlayerHomeZone_RedisFailureIsUnavailableNotNotFound(t *testing.T) {
	s, mr := newHomeZoneTestServer(t)
	ctx := context.Background()
	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1002, HomeZoneId: 3})
	require.NoError(t, err)

	mr.Close()
	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1002})
	require.Error(t, err)
	assert.Nil(t, resp)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.NotContains(t, st.Message(), "no home zone mapping",
		"Redis 故障绝不能带上'缺失映射'的文案,否则 login 会把故障当成正常状态吞掉")
}

// TestBatchGetPlayerHomeZone_OmitsMissingIds:批量口径刻意与单查不同 ——
// 缺席的 id 直接不出现,不是错误(角色列表天然要处理"部分有部分没有")。
func TestBatchGetPlayerHomeZone_OmitsMissingIds(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 2001, HomeZoneId: 5})
	require.NoError(t, err)

	resp, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{
		PlayerIds: []uint64{2001, 2002},
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(5), resp.GetPlayerZoneMap()[2001])
	_, present := resp.GetPlayerZoneMap()[2002]
	assert.False(t, present, "缺席的 id 直接不出现,不能整批失败")
}

// ── RegisterPlayerZone:SETNX + 合服闸门 ────────────────────────

func TestRegisterPlayerZone_ConflictIsAlreadyExistsWithExistingZone(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3001, HomeZoneId: 3})
	require.NoError(t, err)

	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3001, HomeZoneId: 8})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.AlreadyExists, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeZoneMappingConflict))
	assert.Contains(t, st.Message(), "already mapped to home zone 3", "消息必须带上既有 zone")

	// 零变更
	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 3001})
	require.NoError(t, err)
	assert.Equal(t, uint32(3), resp.GetHomeZoneId())
}

func TestRegisterPlayerZone_SameZoneIsIdempotent(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3002, HomeZoneId: 4})
		require.NoError(t, err, "第 %d 次重复登记同一 zone 必须成功(CreatePlayer 允许重试)", i+1)
	}
}

func TestRegisterPlayerZone_RefusedWhileZoneIsMerging(t *testing.T) {
	s, mr := newHomeZoneTestServer(t)
	ctx := context.Background()

	require.NoError(t, mr.Set(routing.MergeFenceKey(20), `{"started_at":1757000000}`))

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3003, HomeZoneId: 20})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeZoneMergeInProgress))

	// 闸门只封锁被合的那个 zone。
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3004, HomeZoneId: 21})
	require.NoError(t, err)
}

// ── RemapHomeZoneForMerge:鉴权 + 闸门 ──────────────────────────

const testAdminToken = "s3cret-merge-token"

// adminCtx 造一条带 x-admin-token 与对端地址的 incoming context(模拟真实调用)。
func adminCtx(token string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		adminTokenMetadataKey, token,
		"x-operator", "ops-alice",
	))
	return peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 51234}})
}

func newMergeAdminServer(t *testing.T) (*DataServiceServer, *miniredis.Miniredis) {
	t.Helper()
	return newHomeZoneTestServerWithConfig(t, func(c *config.Config) { c.AdminToken = testAdminToken })
}

// TestRemapHomeZoneForMerge_DisabledWhenAdminTokenUnset:没配 token = 停用,
// **不是**免鉴权。这是默认形态,所以必须是最显眼的一条测试。
func TestRemapHomeZoneForMerge_DisabledWhenAdminTokenUnset(t *testing.T) {
	s, mr := newHomeZoneTestServer(t) // AdminToken 留空
	require.NoError(t, mr.Set(routing.MergeFenceKey(1), `{"started_at":1757000000}`))
	require.NoError(t, mr.Set("player:zone:9001", "1"))

	resp, err := s.RemapHomeZoneForMerge(adminCtx(""), &data_service.RemapHomeZoneForMergeRequest{
		SourceZoneId: 1, TargetZoneId: 2,
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeAdminAuthRequired))

	val, err := mr.Get("player:zone:9001")
	require.NoError(t, err)
	assert.Equal(t, "1", val, "被拒绝的 remap 必须零变更")
}

func TestRemapHomeZoneForMerge_RejectsMissingOrWrongToken(t *testing.T) {
	s, mr := newMergeAdminServer(t)
	require.NoError(t, mr.Set(routing.MergeFenceKey(1), `{"started_at":1757000000}`))

	cases := map[string]context.Context{
		"完全没有 metadata": context.Background(),
		"metadata 里没有 token": peer.NewContext(
			metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-operator", "ops-bob")),
			&peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 1}}),
		"token 不对": adminCtx("wrong-token"),
		"token 为空": adminCtx(""),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.RemapHomeZoneForMerge(ctx, &data_service.RemapHomeZoneForMergeRequest{
				SourceZoneId: 1, TargetZoneId: 2,
			})
			require.Error(t, err)
			st, _ := status.FromError(err)
			assert.Equal(t, codes.PermissionDenied, st.Code())
			assert.NotContains(t, st.Message(), testAdminToken, "错误消息不能回显正确 token")
		})
	}
}

// TestRemapHomeZoneForMerge_RefusedWithoutMergeMarker:带对了 token 也不够 ——
// 没有 merge:in_progress:{src} 说明源 zone 仍在线接受新映射,这是"线上误调用"的样子。
func TestRemapHomeZoneForMerge_RefusedWithoutMergeMarker(t *testing.T) {
	s, mr := newMergeAdminServer(t)
	require.NoError(t, mr.Set("player:zone:9002", "1"))

	_, err := s.RemapHomeZoneForMerge(adminCtx(testAdminToken), &data_service.RemapHomeZoneForMergeRequest{
		SourceZoneId: 1, TargetZoneId: 2,
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeMergeFenceMissing))
	assert.Contains(t, st.Message(), "merge:in_progress:1")

	val, _ := mr.Get("player:zone:9002")
	assert.Equal(t, "1", val, "零变更")
}

func TestRemapHomeZoneForMerge_SucceedsWithTokenAndMarker(t *testing.T) {
	s, mr := newMergeAdminServer(t)
	require.NoError(t, mr.Set("player:zone:9003", "1"))
	require.NoError(t, mr.Set("player:zone:9004", "1"))
	require.NoError(t, mr.Set("player:zone:9005", "3"))
	require.NoError(t, mr.Set(routing.MergeFenceKey(1), `{"started_at":1757000000}`))

	// dry-run 先数一遍,零变更
	resp, err := s.RemapHomeZoneForMerge(adminCtx(testAdminToken), &data_service.RemapHomeZoneForMergeRequest{
		SourceZoneId: 1, TargetZoneId: 2, DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.GetErrorCode())
	assert.Equal(t, uint32(2), resp.GetPlayersMatched())
	assert.Equal(t, uint32(0), resp.GetPlayersUpdated())
	val, _ := mr.Get("player:zone:9003")
	assert.Equal(t, "1", val)

	// 再 apply
	resp, err = s.RemapHomeZoneForMerge(adminCtx(testAdminToken), &data_service.RemapHomeZoneForMergeRequest{
		SourceZoneId: 1, TargetZoneId: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(2), resp.GetPlayersMatched())
	assert.Equal(t, uint32(2), resp.GetPlayersUpdated())

	for _, key := range []string{"player:zone:9003", "player:zone:9004"} {
		v, err := mr.Get(key)
		require.NoError(t, err)
		assert.Equal(t, "2", v, key)
	}
	v, _ := mr.Get("player:zone:9005")
	assert.Equal(t, "3", v, "别的 zone 的玩家不许被动到")
}

func TestRemapHomeZoneForMerge_RejectsBadZonesAfterAuth(t *testing.T) {
	s, _ := newMergeAdminServer(t)

	for _, tc := range []struct{ src, dst uint32 }{{0, 2}, {1, 0}, {5, 5}} {
		_, err := s.RemapHomeZoneForMerge(adminCtx(testAdminToken), &data_service.RemapHomeZoneForMergeRequest{
			SourceZoneId: tc.src, TargetZoneId: tc.dst,
		})
		require.Error(t, err, "src=%d dst=%d", tc.src, tc.dst)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.InvalidArgument, st.Code())
		assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeInvalidRequest))
	}
}

// errCodeTag 拼 handler 塞进 gRPC message 的码标记。测试与实现共用同一份常量,
// 断言里绝不出现字面数字 —— 否则改码时测试会"仍然通过"却在线上对不上。
func errCodeTag(code uint32) string {
	return "error_code=" + itoa(code)
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
