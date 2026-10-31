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
	"google.golang.org/protobuf/types/known/emptypb"
)

// memRouteStore 是内存版路由表,判定与 store.RouteStore.RegisterHome 同口径:绝不覆盖,落点只在这次真的写入时钉上。
// SQL 本身由 store 包的契约用例与真库用例钉住,这里只测 server 这一层:routing 的错误怎么翻成 gRPC code + error_code。
type memRouteStore struct {
	rows map[uint64]store.RouteRow
	// err 非 nil 时读写都回它,模拟全局库故障(含 route_meta 冻结)。
	err error
	// failGetRoutesOn 让第 n 次 GetRoutes(从 1 起)失败。
	failGetRoutesOn int
	registerCalls   int
	getRoutesCalls  int
}

// 接口一变,编译期先炸。
var _ routing.RouteStore = (*memRouteStore)(nil)

var errRouteSQL = errors.New("player_route: connection reset by peer")

func (f *memRouteStore) RegisterHome(_ context.Context, req store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	f.registerCalls++
	if f.err != nil {
		return store.RouteRegisterResult{}, f.err
	}
	row, ok := f.rows[req.PlayerID]
	switch {
	case !ok:
		f.rows[req.PlayerID] = store.RouteRow{PlayerID: req.PlayerID, HomeZone: req.HomeZone, StorageID: req.StorageID, Rev: 1}
		return store.RouteRegisterResult{Outcome: store.RouteRegisterInserted}, nil
	case row.HomeZone == req.HomeZone:
		return store.RouteRegisterResult{Outcome: store.RouteRegisterAlreadySame}, nil
	default:
		return store.RouteRegisterResult{Outcome: store.RouteRegisterConflict, ExistingHome: row.HomeZone}, nil
	}
}

// GetRoutes 同真实驱动:ctx 已结束的查询直接失败。
func (f *memRouteStore) GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	f.getRoutesCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.getRoutesCalls == f.failGetRoutesOn {
		return nil, errRouteSQL
	}
	out := make(map[uint64]store.RouteRow, len(ids))
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

// staticMergedZones 建一个固定内容的合服缓存:不 Start,快照就是构造时读到的这一份。
func staticMergedZones(t *testing.T, merges map[uint32]uint32) *placement.MergedZoneCache {
	t.Helper()
	cache, err := placement.NewMergedZoneCache(context.Background(),
		func(context.Context) (map[uint32]uint32, error) { return merges, nil }, placement.MergedZoneCacheOptions{})
	require.NoError(t, err)
	return cache
}

// newHomeZoneTestServer 只装 Router(内存版路由表)与空的合服缓存,其余 store 保持 nil ——
// 归属 RPC 只碰这两样,不需要 MySQL。
func newHomeZoneTestServer(t *testing.T) (*DataServiceServer, *memRouteStore) {
	t.Helper()
	routes := &memRouteStore{rows: map[uint64]store.RouteRow{}}
	return NewDataServiceServer(&svc.ServiceContext{
		Router:      routing.NewRouter(routes),
		MergedZones: staticMergedZones(t, nil),
	}), routes
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

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1001, HomeZoneId: 7, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)

	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1001})
	require.NoError(t, err)
	assert.Equal(t, uint32(7), resp.GetHomeZoneId())
}

// TestGetPlayerHomeZone_StoreFailureIsUnavailableNotNotFound:库故障与「没有映射」必须是两个不同的码,
// 否则一次全局库宕机会伪装成一批"老玩家",合服当天最需要这条信号时它恰好是哑的。
func TestGetPlayerHomeZone_StoreFailureIsUnavailableNotNotFound(t *testing.T) {
	s, routes := newHomeZoneTestServer(t)
	ctx := context.Background()
	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1002, HomeZoneId: 3, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)

	routes.err = errRouteSQL
	resp, err := s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1002})
	require.Error(t, err)
	assert.Nil(t, resp)
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	st, _ := status.FromError(err)
	assert.NotContains(t, st.Message(), "no home zone mapping",
		"库故障绝不能带上'缺失映射'的文案,否则 login 会把故障当成正常状态吞掉")
}

// newMergedZoneTestServer 同 newHomeZoneTestServer,再装一张固定的合服映射。
func newMergedZoneTestServer(t *testing.T, merges map[uint32]uint32) *DataServiceServer {
	t.Helper()
	s, _ := newHomeZoneTestServer(t)
	s.svcCtx.MergedZones = staticMergedZones(t, merges)
	return s
}

// TestHomeZoneRPCs_ResolveToFinalZone:两个归属 RPC 的出口都解析到最终区(沿链,5→6→7),映射里没有的区原样返回;
// 登记写入的是原始值(合服中在源区建号是允许的,他的 home_zone 就是源区),读回来才是目标区。
func TestHomeZoneRPCs_ResolveToFinalZone(t *testing.T) {
	s := newMergedZoneTestServer(t, map[uint32]uint32{5: 6, 6: 7})
	ctx := context.Background()
	for player, zone := range map[uint64]uint32{1101: 5, 1102: 6, 1103: 7, 1104: 9} {
		_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: player, HomeZoneId: zone, StorageId: placement.DefaultGlobalStorageID})
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
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1101, HomeZoneId: 5, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)
}

// TestHomeZoneRPCs_MissingCacheFailsClosed:缓存没装配(main 本该拒启,这是纵深防御)时不能当成没有合服。
func TestHomeZoneRPCs_MissingCacheFailsClosed(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()
	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1301, HomeZoneId: 5, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)
	s.svcCtx.MergedZones = nil

	_, err = s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1301})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	_, err = s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{1301}})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
}

// TestGetMergedZones:回答缓存快照里的全部映射(src → dst,不沿链解析);没有合服是一个答案,回空;
// 缓存没装配回 Unavailable + 码 30,调用方不能把「读不到」当成「没有合服」。
func TestGetMergedZones(t *testing.T) {
	ctx := context.Background()

	s := newMergedZoneTestServer(t, map[uint32]uint32{5: 6, 6: 7})
	resp, err := s.GetMergedZones(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{5: 6, 6: 7}, resp.GetMergedInto())

	s = newMergedZoneTestServer(t, nil)
	resp, err = s.GetMergedZones(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetMergedInto())

	s.svcCtx.MergedZones = nil
	resp, err = s.GetMergedZones(ctx, &emptypb.Empty{})
	assert.Nil(t, resp)
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
}

// TestHomeZoneRPCs_NoRouteStoreFailsClosed:route store 没装配时读写都回 Unavailable + 码 30,绝不 NotFound。
func TestHomeZoneRPCs_NoRouteStoreFailsClosed(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	s.svcCtx.Router = routing.NewRouter(nil)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 1401, HomeZoneId: 5, StorageId: placement.DefaultGlobalStorageID})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	_, err = s.GetPlayerHomeZone(ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 1401})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	_, err = s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{1401}})
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
}

// TestBatchGetPlayerHomeZone_OmitsMissingIds:批量口径刻意与单查不同 ——
// 缺席的 id 直接不出现,不是错误(角色列表天然要处理"部分有部分没有")。
func TestBatchGetPlayerHomeZone_OmitsMissingIds(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 2001, HomeZoneId: 5, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)

	resp, err := s.BatchGetPlayerHomeZone(ctx, &data_service.BatchGetPlayerHomeZoneRequest{
		PlayerIds: []uint64{2001, 2002},
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(5), resp.GetPlayerZoneMap()[2001])
	_, present := resp.GetPlayerZoneMap()[2002]
	assert.False(t, present, "缺席的 id 直接不出现,不能整批失败")
}

// bigHomeZoneBatch 造一批跨过单批上限的 id,读路径要分两块。
func bigHomeZoneBatch() []uint64 {
	ids := make([]uint64, store.RouteBatchLimit+5)
	for i := range ids {
		ids[i] = uint64(610001 + i)
	}
	return ids
}

// TestBatchGetPlayerHomeZone_AnswersAcrossChunks:跨过单批上限时分块读、结果合并。
func TestBatchGetPlayerHomeZone_AnswersAcrossChunks(t *testing.T) {
	s, routes := newHomeZoneTestServer(t)
	ids := bigHomeZoneBatch()
	first, last := ids[0], ids[store.RouteBatchLimit]
	routes.rows[first] = store.RouteRow{PlayerID: first, HomeZone: 9, Rev: 1}
	routes.rows[last] = store.RouteRow{PlayerID: last, HomeZone: 11, Rev: 1}

	resp, err := s.BatchGetPlayerHomeZone(context.Background(), &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: ids})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{first: 9, last: 11}, resp.GetPlayerZoneMap())
	assert.Equal(t, 2, routes.getRoutesCalls)
}

// TestBatchGetPlayerHomeZone_AnyChunkFailureFailsWholeBatch:任一块读库失败整批 Unavailable + 码 30,不回半份:
// 半份会被调用方当成「这些人没有归属」。
func TestBatchGetPlayerHomeZone_AnyChunkFailureFailsWholeBatch(t *testing.T) {
	s, routes := newHomeZoneTestServer(t)
	ids := bigHomeZoneBatch()
	routes.rows[ids[0]] = store.RouteRow{PlayerID: ids[0], HomeZone: 9, Rev: 1}
	routes.failGetRoutesOn = 2

	resp, err := s.BatchGetPlayerHomeZone(context.Background(), &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: ids})
	require.Error(t, err)
	assert.Nil(t, resp, "no partial map")
	assertStatusCode(t, err, codes.Unavailable, constants.ErrCodeRouteStoreError)
	assert.Equal(t, 2, routes.getRoutesCalls)
}

// ── RegisterPlayerZone:绝不覆盖 ────────────────────────

func TestRegisterPlayerZone_ConflictIsAlreadyExistsWithExistingZone(t *testing.T) {
	s, _ := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3001, HomeZoneId: 3, StorageId: placement.DefaultGlobalStorageID})
	require.NoError(t, err)

	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3001, HomeZoneId: 8, StorageId: placement.DefaultGlobalStorageID})
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
		_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3002, HomeZoneId: 4, StorageId: placement.DefaultGlobalStorageID})
		require.NoError(t, err, "第 %d 次重复登记同一 zone 必须成功(CreatePlayer 允许重试)", i+1)
	}
}

// TestRegisterPlayerZone_StorageIdPinsPlacementOnlyOnFirstWrite:storage_id 经 RPC 透传到库;
// 冲突被拒时落点零变更。
func TestRegisterPlayerZone_StorageIdPinsPlacementOnlyOnFirstWrite(t *testing.T) {
	s, routes := newHomeZoneTestServer(t)
	ctx := context.Background()

	_, err := s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{
		PlayerId: 3101, HomeZoneId: 4, StorageId: placement.DefaultGlobalStorageID,
	})
	require.NoError(t, err)
	assert.Equal(t, placement.DefaultGlobalStorageID, routes.rows[3101].StorageID)

	const firstStorage = uint32(11)
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{PlayerId: 3102, HomeZoneId: 4, StorageId: firstStorage})
	require.NoError(t, err)
	_, err = s.RegisterPlayerZone(ctx, &data_service.RegisterPlayerZoneRequest{
		PlayerId: 3102, HomeZoneId: 8, StorageId: placement.DefaultGlobalStorageID,
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.AlreadyExists, st.Code())
	assert.Equal(t, firstStorage, routes.rows[3102].StorageID, "冲突的登记不得改落点")
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

// ── RegisterPlayerZone 的错误映射 ──

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

// TestRegisterPlayerZone_ErrorMapping:gRPC code 只有 OK / AlreadyExists / Unavailable,
// 码 30(库故障)与码 14(参数非法)只体现在 message 里。
func TestRegisterPlayerZone_ErrorMapping(t *testing.T) {
	const playerID, zone = uint64(5101), uint32(7)

	cases := []struct {
		name     string
		playerID uint64
		home     uint32
		setup    func(routes *memRouteStore)
		code     codes.Code
		errCode  uint32
		noStore  bool // 参数非法不许碰库
	}{
		{name: "新玩家:提交即完成", playerID: playerID, home: zone, code: codes.OK},
		{
			name: "库里是同一个 home:幂等", playerID: playerID, home: zone,
			setup: func(routes *memRouteStore) {
				routes.rows[playerID] = store.RouteRow{PlayerID: playerID, HomeZone: zone, Rev: 1}
			},
			code: codes.OK,
		},
		{
			name: "库里是别的 home", playerID: playerID, home: zone,
			setup: func(routes *memRouteStore) {
				routes.rows[playerID] = store.RouteRow{PlayerID: playerID, HomeZone: 3, Rev: 1}
			},
			code: codes.AlreadyExists, errCode: constants.ErrCodeZoneMappingConflict,
		},
		{
			name: "库故障(route_meta 冻结)", playerID: playerID, home: zone,
			setup: func(routes *memRouteStore) { routes.err = store.ErrRouteNotWritable },
			code:  codes.Unavailable, errCode: constants.ErrCodeRouteStoreError,
		},
		{
			name: "参数非法(player_id=0)", playerID: 0, home: zone,
			code: codes.Unavailable, errCode: constants.ErrCodeInvalidRequest, noStore: true,
		},
		{
			name: "参数非法(home 越界)", playerID: playerID, home: store.RouteHomeZoneMax + 1,
			code: codes.Unavailable, errCode: constants.ErrCodeInvalidRequest, noStore: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, routes := newHomeZoneTestServer(t)
			if tc.setup != nil {
				tc.setup(routes)
			}
			_, err := s.RegisterPlayerZone(context.Background(), &data_service.RegisterPlayerZoneRequest{
				PlayerId: tc.playerID, HomeZoneId: tc.home, StorageId: placement.DefaultGlobalStorageID,
			})
			if tc.code == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, zone, routes.rows[playerID].HomeZone)
				return
			}
			require.Error(t, err)
			assertStatusCode(t, err, tc.code, tc.errCode)
			if tc.noStore {
				assert.Zero(t, routes.registerCalls, "参数非法不许碰库")
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

// newPlayerNameTestServer 装名字缓存(指向 miniredis)+ 假 store,配置走**规范化后的默认值**
// —— 与线上一致的 10m / 24h / 60s,这样窗口断言用的是真正的默认窗口。
func newPlayerNameTestServer(t *testing.T, tweak func(*config.Config)) (*DataServiceServer, *fakePlayerNameStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		NameCacheRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		PlayerName:     config.PlayerNameConfig{}.Normalize(),
	}
	if tweak != nil {
		tweak(&c)
	}

	fake := &fakePlayerNameStore{
		reserveOutcome: store.ReserveInserted,
		releaseOutcome: store.ReleaseDeleted,
		names:          map[uint64]string{},
	}
	return NewDataServiceServer(&svc.ServiceContext{Config: c, PlayerNameCache: routing.NewPlayerNameCache(c.NameCacheRedis), PlayerNameStore: fake}), fake
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
