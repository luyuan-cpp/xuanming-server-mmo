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

	"shared/placement"
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
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
	}
	tweak(&c)
	r := routing.NewRouter(c)
	t.Cleanup(r.Close)
	return NewDataServiceServer(&svc.ServiceContext{Config: c, Router: r, MergedZones: routing.NewStaticMergedZoneCache(nil)}), mr
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

// newMergedZoneTestServer 同 newHomeZoneTestServer,再装一张固定的合服映射。
func newMergedZoneTestServer(t *testing.T, merges map[uint32]uint32) *DataServiceServer {
	t.Helper()
	s, _ := newHomeZoneTestServer(t)
	s.svcCtx.MergedZones = routing.NewStaticMergedZoneCache(merges)
	return s
}

// TestHomeZoneRPCs_ResolveToFinalZone:两个归属 RPC 的出口都解析到最终区(沿链,5→6→7),映射里没有的区原样返回;
// 登记写入的是原始值(合服中在源区建号是允许的,他的 home_zone 就是源区),读回来才是目标区。
func TestHomeZoneRPCs_ResolveToFinalZone(t *testing.T) {
	s := newMergedZoneTestServer(t, map[uint32]uint32{5: 6, 6: 7})
	ctx := context.Background()
	for player, zone := range map[uint64]uint32{1101: 5, 1102: 6, 1103: 7, 1104: 9} {
		_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: player, HomeZoneId: zone})
		require.NoError(t, err, "registering into a merged source zone is allowed")
	}

	for player, want := range map[uint64]uint32{1101: 7, 1102: 7, 1103: 7, 1104: 9} {
		resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: player})
		require.NoError(t, err)
		assert.Equal(t, want, resp.GetHomeZoneId(), "player %d", player)
	}

	batch, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{1101, 1102, 1103, 1104, 1105}})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{1101: 7, 1102: 7, 1103: 7, 1104: 9}, batch.GetPlayerZoneMap(), "absent ids stay absent")

	// 幂等重试按原始值比:重试同一个源区照旧成功,不被解析后的值判成冲突。
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1101, HomeZoneId: 5})
	require.NoError(t, err)
}

// TestHomeZoneRPCs_UnresolvableMergeFailsClosed:映射成环解析不出最终区时回 Unavailable(不是 NotFound,也不回原始值),
// 批量整批失败、不回半份;别的区不受影响。
func TestHomeZoneRPCs_UnresolvableMergeFailsClosed(t *testing.T) {
	s := newMergedZoneTestServer(t, map[uint32]uint32{5: 6, 6: 5})
	ctx := context.Background()
	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1201, HomeZoneId: 5})
	require.NoError(t, err)
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1202, HomeZoneId: 9})
	require.NoError(t, err)

	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1201})
	assert.Nil(t, resp)
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)

	batch, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{1201, 1202}})
	assert.Nil(t, batch, "no partial map")
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)

	ok, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1202})
	require.NoError(t, err)
	assert.Equal(t, uint32(9), ok.GetHomeZoneId())
}

// TestHomeZoneRPCs_MissingCacheFailsClosed:缓存没装配(main 本该拒启,这是纵深防御)时不能当成没有合服。
func TestHomeZoneRPCs_MissingCacheFailsClosed(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()
	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1301, HomeZoneId: 5})
	require.NoError(t, err)
	s.svcCtx.MergedZones = nil

	_, err = s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1301})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	_, err = s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{1301}})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
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

// ── RegisterPlayerZone:SETNX ────────────────────────

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

// TestRegisterPlayerZone_StorageIdPinsPlacementOnlyOnFirstWrite:storage_id 经 RPC 透传到
// 同一段 Lua;冲突被拒时落点零变更,拒绝码与不带 storage_id 时相同。
func TestRegisterPlayerZone_StorageIdPinsPlacementOnlyOnFirstWrite(t *testing.T) {
	s, mr := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{
		PlayerId: 3101, HomeZoneId: 4, StorageId: placement.DefaultGlobalStorageID,
	})
	require.NoError(t, err)
	raw, err := mr.Get(placement.Key(3101))
	require.NoError(t, err)
	assert.Equal(t, placement.StableValue(placement.DefaultGlobalStorageID, 1), raw)

	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3102, HomeZoneId: 4})
	require.NoError(t, err)
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{
		PlayerId: 3102, HomeZoneId: 8, StorageId: placement.DefaultGlobalStorageID,
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.AlreadyExists, st.Code())
	assert.False(t, mr.Exists(placement.Key(3102)), "冲突的登记不得留下落点")
}

// ── admin token 的公共辅助 ──────────

const testAdminToken = "s3cret-merge-token"

// adminCtx 造一条带 x-admin-token 与对端地址的 incoming context(模拟真实调用)。
func adminCtx(token string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		adminTokenMetadataKey, token,
		"x-operator", "ops-alice",
	))
	return peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 51234}})
}

// ── RegisterPlayerZone 的 db_first 错误映射(docs/design/player-route-mapping-durability-proposal.md §4,规格 c.3)──
//
// 这一组只测 server 这一层:routing 的错误怎么翻成 gRPC code + error_code。编排本身由 routing 的
// route_publish_test.go 覆盖,库的行为由 store 的契约 / 真库用例覆盖,这里用假 store 挡在外面。

// fakeRouteStore 按 routing.RouteStore 注入,记调用次数。
type fakeRouteStore struct {
	result store.RouteRegisterResult
	err    error
	calls  int
}

// 接口一变,编译期先炸。
var _ routing.RouteStore = (*fakeRouteStore)(nil)

func (f *fakeRouteStore) RegisterHome(_ context.Context, _ store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	f.calls++
	if f.err != nil {
		return store.RouteRegisterResult{}, f.err
	}
	return f.result, nil
}

func intPtr(v int) *int { return &v }

// newDBFirstTestServer 同 newHomeZoneTestServerWithConfig,但 RouteStore **显式**写 db_first 并注入假 store。
// WAITAOF 两项显式写 0:miniredis 是否支持 WAITAOF 没有核实(规格 Q4),真实的 WAITAOF 只在 routing 的真 Redis 集成测试里验。
func newDBFirstTestServer(t *testing.T, fake routing.RouteStore) (*DataServiceServer, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		RouteStore: config.RouteStoreConfig{
			Mode:               config.RouteStoreModeDBFirst,
			WaitAOFNumLocal:    intPtr(0),
			WaitAOFNumReplicas: intPtr(0),
		},
	}
	r := routing.NewRouter(c, routing.WithRouteStore(fake))
	t.Cleanup(r.Close)
	return NewDataServiceServer(&svc.ServiceContext{Config: c, Router: r, MergedZones: routing.NewStaticMergedZoneCache(nil)}), mr
}

// assertStatusCode 断言 err 是 gRPC status、code 相符、message 以 "error_code=<n>:" 开头。
// 按前缀加冒号比,免得 "error_code=1" 误配 "error_code=14"。
func assertStatusCode(t *testing.T, err error, code codes.Code, errCode uint32) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a gRPC status: %v", err)
	assert.Equal(t, code, st.Code(), st.Message())
	assert.True(t, strings.HasPrefix(st.Message(), errCodeTag(errCode)+":"),
		"message %q must start with %q", st.Message(), errCodeTag(errCode)+":")
}

// TestDBFirstRegister_ErrorMapping:db_first 下 gRPC code 的集合与 redis 模式完全相同(OK / AlreadyExists /
// Unavailable),新增的码 30(库故障)与码 14(参数非法)只体现在 message 里。
func TestDBFirstRegister_ErrorMapping(t *testing.T) {
	const playerID, zone = uint64(5101), uint32(7)
	committed := func(outcome store.RouteRegisterOutcome, row store.RouteRow) store.RouteRegisterResult {
		row.PlayerID, row.HomeZone = playerID, zone
		if row.Rev == 0 {
			row.Rev = 1
		}
		return store.RouteRegisterResult{Outcome: outcome, Row: row}
	}

	cases := []struct {
		name     string
		playerID uint64
		fake     *fakeRouteStore
		setup    func(mr *miniredis.Miniredis, fake *fakeRouteStore)
		code     codes.Code
		errCode  uint32
	}{
		{
			name: "新玩家:提交并发布", playerID: playerID,
			fake: &fakeRouteStore{result: committed(store.RouteRegisterInserted, store.RouteRow{})},
			code: codes.OK,
		},
		{
			name: "库里是别的 home", playerID: playerID,
			fake: &fakeRouteStore{result: store.RouteRegisterResult{Outcome: store.RouteRegisterConflict, ExistingHome: 3}},
			code: codes.AlreadyExists, errCode: constants.ErrCodeZoneMappingConflict,
		},
		{
			name: "库故障(route_meta 冻结)", playerID: playerID,
			fake: &fakeRouteStore{err: store.ErrRouteNotWritable},
			code: codes.Unavailable, errCode: constants.ErrCodeRouteStoreError,
		},
		{
			name: "缓存认得玩家而库里无行", playerID: playerID,
			fake: &fakeRouteStore{result: store.RouteRegisterResult{Outcome: store.RouteRegisterRowMissing}},
			setup: func(mr *miniredis.Miniredis, _ *fakeRouteStore) {
				_ = mr.Set(placement.Key(playerID), placement.StableValue(placement.DefaultGlobalStorageID, 1))
			},
			code: codes.Unavailable, errCode: constants.ErrCodeRouteStoreError,
		},
		{
			name: "发布失败(畸形 maprev,脚本拒绝)", playerID: playerID,
			fake: &fakeRouteStore{result: committed(store.RouteRegisterAlreadySame, store.RouteRow{})},
			setup: func(mr *miniredis.Miniredis, _ *fakeRouteStore) {
				_ = mr.Set(placement.MapRevKey(playerID), "not-a-rev")
			},
			code: codes.Unavailable, errCode: constants.ErrCodeRedis,
		},
		{
			name: "遗留值冲突(无 maprev,缓存落点与库不同)", playerID: playerID,
			fake: &fakeRouteStore{result: committed(store.RouteRegisterAlreadySame,
				store.RouteRow{StorageID: placement.DefaultGlobalStorageID, PlacementVersion: 1})},
			setup: func(mr *miniredis.Miniredis, _ *fakeRouteStore) {
				_ = mr.Set(placement.Key(playerID), "7:1")
			},
			code: codes.Unavailable, errCode: constants.ErrCodeRedis,
		},
		{
			name: "参数非法(player_id=0)", playerID: 0,
			fake: &fakeRouteStore{result: committed(store.RouteRegisterInserted, store.RouteRow{})},
			code: codes.Unavailable, errCode: constants.ErrCodeInvalidRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newDBFirstTestServer(t, tc.fake)
			if tc.setup != nil {
				tc.setup(mr, tc.fake)
			}
			_, err := s.RegisterPlayerZone(context.Background(), &data_service.RegisterPlayerZoneRequest{
				PlayerId: tc.playerID, HomeZoneId: zone,
			})
			if tc.code == codes.OK {
				require.NoError(t, err)
				got, err := mr.Get(placement.HomeZoneKey(playerID))
				require.NoError(t, err)
				assert.Equal(t, "7", got)
				rev, err := mr.Get(placement.MapRevKey(playerID))
				require.NoError(t, err)
				assert.Equal(t, "1", rev)
				return
			}
			require.Error(t, err)
			assertStatusCode(t, err, tc.code, tc.errCode)
			if tc.errCode == constants.ErrCodeInvalidRequest {
				assert.Zero(t, tc.fake.calls, "参数非法不许碰库")
			}
		})
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
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		PlayerName:   config.PlayerNameConfig{}.Normalize(),
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

// ── 三个 Rollback* 一律要 x-admin-token(docs/design/guild-phase2/07-rollback-fail-closed.md §7.6.3 Q2 = ②,D8 的 token 半边)──

// ── 第 3 批读方门禁的协议形状(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.2、§13.2 第 6 条)──
//
// 只测 server 这一层:routing 回什么 → gRPC code + error_code。编排由 routing 的 route_read_test.go 覆盖。
// mapping Redis 用 miniredis:它不支持 INFO server,读脚本在它上面必然报错 —— 开关打开时正好就是「缓存不可信 → 回源库」,
// 不需要再造标记。回源读者用假实现挡在外面。

// readGateFakeReader 是 routing.RouteReader 的假实现。err 非 nil 时三个方法都回它;failGetRoutesOn 让第 n 次 GetRoutes
// (从 1 起)失败。calls 记三个方法一共被调了几次(开关关闭时必须为 0)。
type readGateFakeReader struct {
	rows            map[uint64]store.RouteRow
	err             error
	failGetRoutesOn int
	getRoutes       int
	calls           int
}

var _ routing.RouteReader = (*readGateFakeReader)(nil)

var errReadGateSQL = errors.New("player_route: connection reset by peer")

func (f *readGateFakeReader) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	f.calls++
	f.getRoutes++
	if f.err != nil {
		return nil, f.err
	}
	if f.failGetRoutesOn != 0 && f.getRoutes == f.failGetRoutesOn {
		return nil, errReadGateSQL
	}
	out := map[uint64]store.RouteRow{}
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func (f *readGateFakeReader) ScanRoutesByHome(context.Context, uint32, store.RouteScanCursor, int) ([]store.RouteRow, error) {
	f.calls++
	return nil, f.err
}

// newReadGateTestServer 同 newDBFirstTestServer,另按 on 设 ReadRequiresReady;reader 非 nil 时注入回源读者
// (不把 nil 的 *readGateFakeReader 传进去:那是「非 nil 接口包着 nil 指针」)。
func newReadGateTestServer(t *testing.T, on bool, reader *readGateFakeReader) (*DataServiceServer, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		RouteStore: config.RouteStoreConfig{
			Mode:               config.RouteStoreModeDBFirst,
			WaitAOFNumLocal:    intPtr(0),
			WaitAOFNumReplicas: intPtr(0),
			ReadRequiresReady:  on,
		},
	}
	var opts []routing.RouterOption
	if reader != nil {
		opts = append(opts, routing.WithRouteReader(reader))
	}
	r := routing.NewRouter(c, opts...)
	t.Cleanup(r.Close)
	return NewDataServiceServer(&svc.ServiceContext{Config: c, Router: r, MergedZones: routing.NewStaticMergedZoneCache(nil)}), mr
}

// TestReadGate_ProtocolShapeWhenCacheUntrusted:ReadRequiresReady=true 且缓存不可信时
//   - SQL 故障 → Unavailable + ErrCodeRouteStoreError(绝不 NotFound,消息里也不带「缺失映射」的契约文案);
//   - SQL 无行 → NotFound(文案契约不变),哪怕缓存里有一个值;
//   - SQL 有行 → OK,home 取自库;
//   - 回源读者没装配 → Unavailable + ErrCodeRouteStoreError;
//   - 批量任一块 SQL 故障 → 整批 Unavailable + ErrCodeRouteStoreError,不回半份。
func TestReadGate_ProtocolShapeWhenCacheUntrusted(t *testing.T) {
	const player = uint64(6101)
	ctx := context.Background()
	bigBatch := make([]uint64, store.RouteBatchLimit+5)
	for i := range bigBatch {
		bigBatch[i] = uint64(610001 + i)
	}

	single := []struct {
		name    string
		reader  *readGateFakeReader
		code    codes.Code
		errCode uint32
		zone    uint32
	}{
		{name: "SQL failure is Unavailable, never NotFound", reader: &readGateFakeReader{err: errReadGateSQL},
			code: codes.Unavailable, errCode: constants.ErrCodeRouteStoreError},
		{name: "SQL has no row is NotFound", reader: &readGateFakeReader{},
			code: codes.NotFound, errCode: constants.ErrCodeNotFound},
		{name: "SQL row answers OK", reader: &readGateFakeReader{rows: map[uint64]store.RouteRow{
			player: {PlayerID: player, HomeZone: 9, Rev: 1}}}, code: codes.OK, zone: 9},
		{name: "no route reader assembled is Unavailable", reader: nil,
			code: codes.Unavailable, errCode: constants.ErrCodeRouteStoreError},
	}
	for _, tc := range single {
		t.Run("GetPlayerHomeZone/"+tc.name, func(t *testing.T) {
			s, mr := newReadGateTestServer(t, true, tc.reader)
			// 缓存里是另一个值:不可信时它既不能被用,它的存在也不能把「库里没有」变成别的答案。
			require.NoError(t, mr.Set(placement.HomeZoneKey(player), "5"))
			resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: player})
			if tc.code == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, tc.zone, resp.GetHomeZoneId())
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			assertStatusCode(t, err, tc.code, tc.errCode)
			st, _ := status.FromError(err)
			if tc.code == codes.NotFound {
				assert.Contains(t, st.Message(), "no home zone mapping", "the NotFound text is a login contract")
			} else {
				assert.NotContains(t, st.Message(), "no home zone mapping", "a fallback failure must not look like a missing mapping")
			}
		})
	}

	t.Run("BatchGetPlayerHomeZone/any chunk SQL failure fails the whole batch", func(t *testing.T) {
		reader := &readGateFakeReader{failGetRoutesOn: 2, rows: map[uint64]store.RouteRow{
			bigBatch[0]: {PlayerID: bigBatch[0], HomeZone: 9, Rev: 1}}}
		s, _ := newReadGateTestServer(t, true, reader)
		resp, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: bigBatch})
		require.Error(t, err)
		assert.Nil(t, resp, "no partial map")
		assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
		assert.Equal(t, 2, reader.getRoutes)
	})

	t.Run("BatchGetPlayerHomeZone/answers from player_route", func(t *testing.T) {
		reader := &readGateFakeReader{rows: map[uint64]store.RouteRow{
			bigBatch[0]:                     {PlayerID: bigBatch[0], HomeZone: 9, Rev: 1},
			bigBatch[store.RouteBatchLimit]: {PlayerID: bigBatch[store.RouteBatchLimit], HomeZone: 11, Rev: 1},
		}}
		s, mr := newReadGateTestServer(t, true, reader)
		require.NoError(t, mr.Set(placement.HomeZoneKey(bigBatch[1]), "5"))
		resp, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: bigBatch})
		require.NoError(t, err)
		assert.Equal(t, map[uint64]uint32{bigBatch[0]: 9, bigBatch[store.RouteBatchLimit]: 11}, resp.GetPlayerZoneMap(),
			"an untrusted cached value is not returned; absent ids stay absent")
	})

	t.Run("BatchGetPlayerHomeZone/no route reader assembled", func(t *testing.T) {
		s, _ := newReadGateTestServer(t, true, nil)
		resp, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: bigBatch[:2]})
		require.Error(t, err)
		assert.Nil(t, resp)
		assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	})
}

// TestReadGate_OffKeepsTheContract:ReadRequiresReady=false(缺省)时契约与今天逐字相同 —— 哪怕 db_first 且回源读者已装配,
// 两个 RPC 也只读缓存:缺键 NotFound(不去库里找)、mapping Redis 故障 Unavailable + ErrCodeRedis(码 1,不是码 30)。
func TestReadGate_OffKeepsTheContract(t *testing.T) {
	ctx := context.Background()
	reader := &readGateFakeReader{rows: map[uint64]store.RouteRow{6202: {PlayerID: 6202, HomeZone: 9, Rev: 1}}}
	s, mr := newReadGateTestServer(t, false, reader)
	require.NoError(t, mr.Set(placement.HomeZoneKey(6201), "5"))

	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 6201})
	require.NoError(t, err)
	assert.Equal(t, uint32(5), resp.GetHomeZoneId(), "switch off: the cache answers")

	_, err = s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 6202})
	assertStatusCode(t, err, codes.NotFound, constants.ErrCodeNotFound)

	batch, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{6201, 6202}})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{6201: 5}, batch.GetPlayerZoneMap())
	assert.Zero(t, reader.calls, "the route reader must not be consulted while ReadRequiresReady=false")

	mr.Close()
	_, err = s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 6201})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRedis)
	_, err = s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{6201}})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRedis)
	assert.Zero(t, reader.calls)
}
