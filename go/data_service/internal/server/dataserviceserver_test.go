package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"
	"proto/data_service"

	"shared/playername"

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

// ── 玩家名字注册表:§3.1 的 gRPC 映射表 ─────────────────────────
//
// 这一组测的**只有 server 这一层**:参数怎么翻成 status/error_code、metadata 里的
// x-admin-token 怎么决定 admin 与否。名字规则、判重、缓存分别由 shared/playername、
// store 的集成测试与 logic 单测覆盖,这里用假 store 把它们挡在外面。

// fakePlayerNameStore 按 svc.PlayerNameStore 注入。它记下**入参**,因为这一层最容易
// 悄悄错的地方就是"传给 store 的 minCreatedMs 是不是 0"—— 那个 0 等于不限时间,
// 而它本该只出现在过了 authorizeAdmin 的调用上。
type fakePlayerNameStore struct {
	reserveOutcome store.ReserveOutcome
	reserveOwner   uint64
	reserveErr     error
	reserveCalls   int
	lastName       string // Reserve 收到的展示名
	lastNorm       string // Reserve 收到的判重键

	releaseOutcome   store.ReleaseOutcome
	releaseErr       error
	releaseCalls     int
	lastMinCreatedMs uint64

	names        map[uint64]string
	batchErr     error
	batchCalls   int
	lastBatchIds []uint64
}

// 接口一变,编译期先炸,不用等到运行时。
var _ svc.PlayerNameStore = (*fakePlayerNameStore)(nil)

func (f *fakePlayerNameStore) Close() error { return nil }

func (f *fakePlayerNameStore) Reserve(_ context.Context, _ uint64, name, norm string, _ uint64) (store.ReserveOutcome, uint64, error) {
	f.reserveCalls++
	f.lastName, f.lastNorm = name, norm
	if f.reserveErr != nil {
		return 0, 0, f.reserveErr
	}
	return f.reserveOutcome, f.reserveOwner, nil
}

func (f *fakePlayerNameStore) Release(_ context.Context, _ uint64, _ string, minCreatedMs uint64) (store.ReleaseOutcome, error) {
	f.releaseCalls++
	f.lastMinCreatedMs = minCreatedMs
	if f.releaseErr != nil {
		return 0, f.releaseErr
	}
	return f.releaseOutcome, nil
}

func (f *fakePlayerNameStore) BatchGet(_ context.Context, ids []uint64) (map[uint64]string, error) {
	f.batchCalls++
	f.lastBatchIds = append([]uint64(nil), ids...)
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := map[uint64]string{}
	for _, id := range ids {
		if name, ok := f.names[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

// newPlayerNameTestServer 装 Router(指向 miniredis)+ 假 store,配置走**规范化后的默认值**
// —— 与线上一致的 10m / 24h / 60s,这样窗口断言用的是真正的默认窗口。
func newPlayerNameTestServer(t *testing.T, tweak func(*config.Config)) (*DataServiceServer, *fakePlayerNameStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis:     redis.RedisConf{Host: mr.Addr(), Type: "node"},
		DevRedis:         config.DevRedisConfig{Host: mr.Addr()},
		PlayerLockTTLSec: 3,
		PlayerName:       config.PlayerNameConfig{}.Normalize(),
	}
	if tweak != nil {
		tweak(&c)
	}
	r := routing.NewRouter(c)
	t.Cleanup(r.Close)

	fake := &fakePlayerNameStore{
		reserveOutcome: store.ReserveInserted,
		releaseOutcome: store.ReleaseDeleted,
		names:          map[uint64]string{},
	}
	return NewDataServiceServer(&svc.ServiceContext{Config: c, Router: r, PlayerNameStore: fake}), fake
}

// ── Reserve ────────────────────────────────────────────────────

// player_id=0 是纵深防御的第一道:这种行一旦插进去就永远没有主人、也没人会释放,
// 却照样占着一个名字。
func TestReservePlayerName_ZeroPlayerIdIsInvalidArgument(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 0, Name: "云中君",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeInvalidRequest))
	assert.Equal(t, 0, fake.reserveCalls, "参数就不合法,不许碰库")
}

// 名字不合规是**业务结果**不是错误:login 要拿 result=2 去翻 tip 文案。
// 而且它必须在碰库之前判完 —— 纯计算,库挂着也能给确定答案。
func TestReservePlayerName_InvalidNameIsResultNotError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空白名", "   "},
		{"含非法字符", "云中君!"},
		{"敏感词", "官方客服"},
		// 结构上限由代码给(playername.StructuralMaxRunes=32),不是配表里的 2–12:
		// 玩法长度归 login 把关,data_service 只保证"存得下"。
		{"超过结构上限", strings.Repeat("云", playername.StructuralMaxRunes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newPlayerNameTestServer(t, nil)
			resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
				PlayerId: 1001, Name: tc.raw,
			})
			require.NoError(t, err)
			assert.Equal(t, playername.ReserveInvalid, resp.GetResult())
			assert.Equal(t, uint64(0), resp.GetOwnerPlayerId())
			assert.Equal(t, 0, fake.reserveCalls, "不合规的名字不许碰库")
		})
	}
}

// store 没装配起来时,不合规的名字仍然要回 result=2 —— 顺序反过来的话,
// "名字打错字"会在故障期变成"服务不可用",玩家改一百遍也建不出号。
func TestReservePlayerName_InvalidNameAnsweredEvenWithoutStore(t *testing.T) {
	s, _ := newPlayerNameTestServer(t, nil)
	s.svcCtx.PlayerNameStore = nil

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1002, Name: "   ",
	})
	require.NoError(t, err)
	assert.Equal(t, playername.ReserveInvalid, resp.GetResult())
}

// 成功路径:展示名保留大小写,判重键是小写 —— 两者必须来自同一次归一化。
func TestReservePlayerName_OkPassesDisplayAndNormToStore(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1003, Name: "YunZhong",
	})
	require.NoError(t, err)
	assert.Equal(t, playername.ReserveOK, resp.GetResult())
	assert.Equal(t, uint64(0), resp.GetOwnerPlayerId(), "只有被别人占用时才填 owner")
	assert.Equal(t, 1, fake.reserveCalls)
	assert.Equal(t, "YunZhong", fake.lastName)
	assert.Equal(t, "yunzhong", fake.lastNorm)
}

// 同 id 同名重试是幂等成功(result=0),不是失败:login 丢了响应会原样重发一次。
func TestReservePlayerName_AlreadyOwnedIsSuccess(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.reserveOutcome = store.ReserveAlreadyOwned

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1004, Name: "云中君",
	})
	require.NoError(t, err)
	assert.Equal(t, playername.ReserveOK, resp.GetResult())
}

// 被别人占用:OK + result=1 + owner。owner 只给 login 判"是不是自己上一次建角的号",
// 不下发客户端(否则等于开了个按名字查 player_id 的接口)。
func TestReservePlayerName_TakenCarriesOwner(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.reserveOutcome = store.ReserveTaken
	fake.reserveOwner = 987654321

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1005, Name: "云中君",
	})
	require.NoError(t, err)
	assert.Equal(t, playername.ReserveTaken, resp.GetResult())
	assert.Equal(t, uint64(987654321), resp.GetOwnerPlayerId())
}

// 同 id 已有别名 = ID 复用事故,必须是错误而不是某个 result:
// 让它伪装成业务分支,login 就会去"换个名字重试",把事故掩盖掉。
func TestReservePlayerName_ConflictIsFailedPrecondition(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.reserveOutcome = store.ReserveConflict

	resp, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1006, Name: "云中君",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodePlayerNameConflict))
}

func TestReservePlayerName_StoreUnavailableIsUnavailable(t *testing.T) {
	s, _ := newPlayerNameTestServer(t, nil)
	s.svcCtx.PlayerNameStore = nil

	_, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1007, Name: "云中君",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodePlayerNameDBError))
}

func TestReservePlayerName_SqlErrorIsUnavailable(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.reserveErr = errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")

	_, err := s.ReservePlayerName(context.Background(), &data_service.ReservePlayerNameRequest{
		PlayerId: 1008, Name: "云中君",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodePlayerNameDBError))
}

// ── Release ────────────────────────────────────────────────────

// 没带 token 的释放必须受窗口约束:传给 store 的 minCreatedMs 不能是 0
// (0 = 不限登记时间 = 任何内网进程都能释放在役角色的名字)。
func TestReleasePlayerName_WithoutTokenPassesWindowFloor(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)

	before := time.Now()
	_, err := s.ReleasePlayerName(context.Background(), &data_service.ReleasePlayerNameRequest{
		PlayerId: 2001, Name: "云中君",
	})
	require.NoError(t, err)
	require.Equal(t, 1, fake.releaseCalls)
	assert.NotZero(t, fake.lastMinCreatedMs, "无 token 的释放绝不能传 0(那是不限时间)")

	want := before.Add(-config.DefaultPlayerNameReleaseWindow).UnixMilli()
	diff := int64(fake.lastMinCreatedMs) - want
	assert.LessOrEqual(t, diff, int64(5000), "窗口下界应约等于 now-ReleaseWindow,实得偏差 %d ms", diff)
	assert.GreaterOrEqual(t, diff, int64(-5000), "窗口下界应约等于 now-ReleaseWindow,实得偏差 %d ms", diff)
}

// 带对 token = 运维清孤儿:不限登记时间(minCreatedMs=0)。
func TestReleasePlayerName_AdminTokenLiftsWindow(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, func(c *config.Config) { c.AdminToken = testAdminToken })

	_, err := s.ReleasePlayerName(adminCtx(testAdminToken), &data_service.ReleasePlayerNameRequest{
		PlayerId: 2002, Name: "云中君",
	})
	require.NoError(t, err)
	require.Equal(t, 1, fake.releaseCalls)
	assert.Equal(t, uint64(0), fake.lastMinCreatedMs, "过了 authorizeAdmin 才允许不限时间")
}

// 带了 x-admin-token 却校验不过 → 原样返回 authorizeAdmin 的错误,**绝不**降级成
// 普通释放:降级会把一次鉴权失败变成一次静默的窗口内删除,日志里毫无痕迹。
func TestReleasePlayerName_BadTokenIsRefusedNotDowngraded(t *testing.T) {
	t.Run("token 不对", func(t *testing.T) {
		s, fake := newPlayerNameTestServer(t, func(c *config.Config) { c.AdminToken = testAdminToken })
		_, err := s.ReleasePlayerName(adminCtx("wrong-token"), &data_service.ReleasePlayerNameRequest{
			PlayerId: 2003, Name: "云中君",
		})
		require.Error(t, err)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.PermissionDenied, st.Code())
		assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeAdminAuthRequired))
		assert.NotContains(t, st.Message(), testAdminToken, "错误消息不能回显正确 token")
		assert.Equal(t, 0, fake.releaseCalls, "鉴权失败必须零变更")
	})

	t.Run("服务端没配 AdminToken 即停用", func(t *testing.T) {
		s, fake := newPlayerNameTestServer(t, nil) // AdminToken 留空
		_, err := s.ReleasePlayerName(adminCtx("anything"), &data_service.ReleasePlayerNameRequest{
			PlayerId: 2004, Name: "云中君",
		})
		require.Error(t, err)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.PermissionDenied, st.Code())
		assert.Equal(t, 0, fake.releaseCalls)
	})
}

// 行不存在 = 幂等成功:login 的补偿会发两次(立即 + 约 10s 后),第二次必然打空。
func TestReleasePlayerName_AbsentIsIdempotentSuccess(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.releaseOutcome = store.ReleaseAbsent

	_, err := s.ReleasePlayerName(context.Background(), &data_service.ReleasePlayerNameRequest{
		PlayerId: 2005, Name: "云中君",
	})
	require.NoError(t, err)
}

// 行在但早于窗口且没 token → FailedPrecondition + 复用 ErrCodeAdminAuthRequired
// (语义就是"这一步需要运维凭据")。
func TestReleasePlayerName_OutsideWindowIsFailedPrecondition(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.releaseOutcome = store.ReleaseOutsideWindow

	_, err := s.ReleasePlayerName(context.Background(), &data_service.ReleasePlayerNameRequest{
		PlayerId: 2006, Name: "云中君",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeAdminAuthRequired))
}

func TestReleasePlayerName_InvalidNameIsInvalidArgument(t *testing.T) {
	for _, raw := range []string{"", "   ", "名字!"} {
		s, fake := newPlayerNameTestServer(t, nil)
		_, err := s.ReleasePlayerName(context.Background(), &data_service.ReleasePlayerNameRequest{
			PlayerId: 2007, Name: raw,
		})
		require.Error(t, err, "raw=%q", raw)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.InvalidArgument, st.Code(), "raw=%q", raw)
		assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeInvalidRequest))
		assert.Equal(t, 0, fake.releaseCalls, "归一化后为空/非法的名字不该进库")
	}
}

// ── BatchGet ───────────────────────────────────────────────────

// 缺席的 id 直接不出现,不是错误:早于本功能建的角色、已释放的名字都属于这一类。
func TestBatchGetPlayerName_OmitsAbsentIds(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.names[3001] = "云中君"

	resp, err := s.BatchGetPlayerName(context.Background(), &data_service.BatchGetPlayerNameRequest{
		PlayerIds: []uint64{3001, 3002},
	})
	require.NoError(t, err)
	assert.Equal(t, "云中君", resp.GetNames()[3001])
	_, present := resp.GetNames()[3002]
	assert.False(t, present, "缺席的 id 不出现,不能整批失败")
}

// 超过 store.PlayerNameBatchLimit 个 id 必须被拒,而不是截断:截断会让调用方
// 拿到一份"部分人没名字"的结果却毫不知情。断言里不写字面量 500。
func TestBatchGetPlayerName_RejectsOversizeBatch(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)

	ids := make([]uint64, 0, store.PlayerNameBatchLimit+1)
	for i := 0; i < store.PlayerNameBatchLimit+1; i++ {
		ids = append(ids, uint64(i+1))
	}

	_, err := s.BatchGetPlayerName(context.Background(), &data_service.BatchGetPlayerNameRequest{PlayerIds: ids})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodeInvalidRequest))
	assert.Equal(t, 0, fake.batchCalls, "超限请求不该打到库上")
}

// SQL 失败不回半份结果:半份会被上游当成"这些人确实没名字"缓存并展示出去。
func TestBatchGetPlayerName_SqlErrorReturnsNoPartialResult(t *testing.T) {
	s, fake := newPlayerNameTestServer(t, nil)
	fake.names[3003] = "云中君"
	fake.batchErr = errors.New("invalid connection")

	resp, err := s.BatchGetPlayerName(context.Background(), &data_service.BatchGetPlayerNameRequest{
		PlayerIds: []uint64{3003, 3004},
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), errCodeTag(constants.ErrCodePlayerNameDBError))
}
