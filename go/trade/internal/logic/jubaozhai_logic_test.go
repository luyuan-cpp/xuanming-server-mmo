package logic

// 逻辑层单测:存储、home_zone、发号全部经接口注入 fake,时间经 Deps.Now 固定。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"

	base "proto/common/base"
	tradepb "proto/trade"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"github.com/zeromicro/go-zero/core/service"
)

const (
	testNowMs = uint64(1_800_000_000_000)
	hourMs    = uint64(3_600_000)

	playerA = uint64(1001) // zoneA 卖家
	playerB = uint64(1002) // zoneA 买家
	playerC = uint64(1003) // zoneB
	playerU = uint64(1009) // 没有 home_zone 映射

	zoneA = uint32(1)
	zoneB = uint32(2)

	maxFavoritesForTest = 3
)

var errStoreDown = errors.New("mysql: connection refused")

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type favoriteKey struct{ player, listing uint64 }

type fakeStore struct {
	listings  map[uint64]*tradepb.TradeListingRecord
	favorites map[favoriteKey]uint64 // → created_ms

	// 浏览 / 货架的返回值由用例直接给出,fake 不模拟 SQL 过滤(SQL 本身在 data 包测)。
	total uint64
	page  []*tradepb.TradeListingRecord
	// favoriteCountOverride 非 nil 时 CountFavorites 返回它(测上限而不必真塞满)。
	favoriteCountOverride *uint64

	failOn map[string]error
	calls  []string

	// 整请求预算用例:blockOn[op]=true 时该操作阻塞到 ctx 结束并返回 ctx.Err()(模拟依赖变慢);
	// deadlines 按调用顺序记录每次存储调用收到的 ctx 截止时间(没有截止时间记零值)。
	blockOn   map[string]bool
	deadlines []time.Time

	lastQuery          *data.ListingQuery
	lastSeller         uint64
	lastOffset         uint64
	lastLimit          uint64
	lastFavoriteLookup []uint64
	insertedListings   []*tradepb.TradeListingRecord
	insertedFavorites  []*tradepb.TradeFavoriteRecord
	deletedFavorites   []favoriteKey
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		listings:  map[uint64]*tradepb.TradeListingRecord{},
		favorites: map[favoriteKey]uint64{},
		failOn:    map[string]error{},
		blockOn:   map[string]bool{},
	}
}

func (s *fakeStore) enter(ctx context.Context, op string) error {
	s.calls = append(s.calls, op)
	s.deadlines = append(s.deadlines, deadlineOf(ctx))
	if s.blockOn[op] {
		if _, ok := ctx.Deadline(); !ok {
			// 没有截止时间还阻塞会让用例永久挂起;直接失败,让断言指出预算缺失。
			return errors.New("fake: ctx 没有截止时间,拒绝阻塞")
		}
		<-ctx.Done() // 模拟不响应的依赖:只有预算到期才返回
		return ctx.Err()
	}
	return s.failOn[op]
}

// deadlineOf 返回 ctx 的截止时间;没有截止时间返回零值。
func deadlineOf(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func (s *fakeStore) called(op string) bool {
	for _, c := range s.calls {
		if c == op {
			return true
		}
	}
	return false
}

func (s *fakeStore) CountListings(ctx context.Context, q data.ListingQuery) (uint64, error) {
	if err := s.enter(ctx, "CountListings"); err != nil {
		return 0, err
	}
	s.lastQuery = &q
	return s.total, nil
}

func (s *fakeStore) QueryListings(ctx context.Context, q data.ListingQuery, offset, limit uint64) ([]*tradepb.TradeListingRecord, error) {
	if err := s.enter(ctx, "QueryListings"); err != nil {
		return nil, err
	}
	s.lastQuery, s.lastOffset, s.lastLimit = &q, offset, limit
	return s.page, nil
}

func (s *fakeStore) CountSellerListings(ctx context.Context, seller uint64) (uint64, error) {
	if err := s.enter(ctx, "CountSellerListings"); err != nil {
		return 0, err
	}
	s.lastSeller = seller
	return s.total, nil
}

func (s *fakeStore) QuerySellerListings(ctx context.Context, seller uint64, offset, limit uint64) ([]*tradepb.TradeListingRecord, error) {
	if err := s.enter(ctx, "QuerySellerListings"); err != nil {
		return nil, err
	}
	s.lastSeller, s.lastOffset, s.lastLimit = seller, offset, limit
	return s.page, nil
}

func (s *fakeStore) GetListing(ctx context.Context, id uint64) (*tradepb.TradeListingRecord, error) {
	if err := s.enter(ctx, "GetListing"); err != nil {
		return nil, err
	}
	rec, ok := s.listings[id]
	if !ok {
		return nil, data.ErrListingNotFound
	}
	return rec, nil
}

func (s *fakeStore) InsertListing(ctx context.Context, rec *tradepb.TradeListingRecord) error {
	if err := s.enter(ctx, "InsertListing"); err != nil {
		return err
	}
	s.insertedListings = append(s.insertedListings, rec)
	s.listings[rec.GetListingId()] = rec
	return nil
}

func (s *fakeStore) FavoriteIDs(ctx context.Context, player uint64, ids []uint64) (map[uint64]bool, error) {
	if err := s.enter(ctx, "FavoriteIDs"); err != nil {
		return nil, err
	}
	s.lastFavoriteLookup = ids
	out := map[uint64]bool{}
	for _, id := range ids {
		if _, ok := s.favorites[favoriteKey{player, id}]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func (s *fakeStore) FavoriteExists(ctx context.Context, player, id uint64) (bool, error) {
	if err := s.enter(ctx, "FavoriteExists"); err != nil {
		return false, err
	}
	_, ok := s.favorites[favoriteKey{player, id}]
	return ok, nil
}

func (s *fakeStore) CountFavorites(ctx context.Context, player uint64) (uint64, error) {
	if err := s.enter(ctx, "CountFavorites"); err != nil {
		return 0, err
	}
	if s.favoriteCountOverride != nil {
		return *s.favoriteCountOverride, nil
	}
	var n uint64
	for k := range s.favorites {
		if k.player == player {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) InsertFavorite(ctx context.Context, rec *tradepb.TradeFavoriteRecord) error {
	if err := s.enter(ctx, "InsertFavorite"); err != nil {
		return err
	}
	s.insertedFavorites = append(s.insertedFavorites, rec)
	s.favorites[favoriteKey{rec.GetPlayerId(), rec.GetListingId()}] = rec.GetCreatedMs()
	return nil
}

func (s *fakeStore) DeleteFavorite(ctx context.Context, player, id uint64) error {
	if err := s.enter(ctx, "DeleteFavorite"); err != nil {
		return err
	}
	s.deletedFavorites = append(s.deletedFavorites, favoriteKey{player, id})
	delete(s.favorites, favoriteKey{player, id})
	return nil
}

type fakeHomeZones struct {
	zones     map[uint64]uint32
	err       error
	calls     []uint64
	deadlines []time.Time
}

func (f *fakeHomeZones) HomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	f.calls = append(f.calls, playerID)
	f.deadlines = append(f.deadlines, deadlineOf(ctx))
	if f.err != nil {
		return 0, f.err
	}
	return f.zones[playerID], nil
}

type fakeListingIDs struct {
	id    uint64
	err   error
	calls int
}

func (f *fakeListingIDs) Next(context.Context) (uint64, error) {
	f.calls++
	return f.id, f.err
}

type fixture struct {
	deps  Deps
	store *fakeStore
	homes *fakeHomeZones
	ids   *fakeListingIDs
}

func newFixture(t *testing.T, scope string) *fixture {
	t.Helper()
	logtest.Discard(t) // 故障分支会打 Error 日志,这里只关心行为
	store := newFakeStore()
	homes := &fakeHomeZones{zones: map[uint64]uint32{playerA: zoneA, playerB: zoneA, playerC: zoneB}}
	ids := &fakeListingIDs{id: 5001}
	cfg := config.Config{
		Market: config.MarketConf{
			Scope: scope, DefaultPageSize: 20, MaxPageSize: 20, MaxPage: 100, MaxFavoritesPerPlayer: maxFavoritesForTest,
		},
	}
	cfg.Mode = service.DevMode
	cfg.Timeout = 4000 // 与 etc/trade.yaml 同值:整请求预算 3500ms
	return &fixture{
		deps: Deps{
			Config:     cfg,
			Store:      store,
			HomeZones:  homes,
			ListingIDs: ids,
			Now:        func() time.Time { return time.UnixMilli(int64(testNowMs)) },
		},
		store: store,
		homes: homes,
		ids:   ids,
	}
}

func (f *fixture) as(player uint64) *JubaozhaiLogic {
	return NewJubaozhaiLogic(playerCtx(player), f.deps)
}

func playerCtx(player uint64) context.Context {
	return session.WithDetails(context.Background(), &base.SessionDetails{PlayerId: player})
}

// onSaleListing:卖家 seller 在 zone 上架、寄售中的武器。
func onSaleListing(id, seller uint64, zone uint32) *tradepb.TradeListingRecord {
	return &tradepb.TradeListingRecord{
		ListingId: id, SellerPlayerId: seller, SellerAccount: "acc", MarketZone: zone, SellerZoneAtListing: zone,
		Category: tradepb.ListingCategory_LISTING_CATEGORY_WEAPON, Subcategory: 1,
		Title: "青锋剑", Level: 10, PriceFen: 500, Status: tradepb.ListingStatus_LISTING_STATUS_LISTED,
		Summary: "摘要", Description: "详细描述", IconKey: "icon_sword",
		NoticeEndMs: testNowMs - hourMs, SaleEndMs: testNowMs + hourMs, CreatedMs: testNowMs - 2*hourMs,
	}
}

func browseRequest() *tradepb.BrowseListingsRequest {
	return &tradepb.BrowseListingsRequest{
		Tab:      tradepb.ListingTab_LISTING_TAB_ON_SALE,
		Section:  tradepb.ListingSection_LISTING_SECTION_CONSIGNMENT,
		Category: tradepb.ListingCategory_LISTING_CATEGORY_WEAPON,
	}
}

// ---------------------------------------------------------------------------
// 无会话
// ---------------------------------------------------------------------------

func TestClientMethodsWithoutSessionRejected(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"缺会话":            context.Background(),
		"会话 player_id=0": session.WithDetails(context.Background(), &base.SessionDetails{SessionId: 7}),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			l := NewJubaozhaiLogic(ctx, f.deps)

			browse, err := l.BrowseListings(browseRequest())
			require.NoError(t, err, "业务失败必须 in-band")
			assert.Equal(t, constants.ErrInvalidParameter, browse.GetErrorMessage().GetId())

			detail, err := l.GetListingDetail(&tradepb.GetListingDetailRequest{ListingId: 1})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, detail.GetErrorMessage().GetId())

			fav, err := l.SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 1, Favorite: true})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, fav.GetErrorMessage().GetId())

			shelf, err := l.GetMyShelf(&tradepb.GetMyShelfRequest{})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, shelf.GetErrorMessage().GetId())

			assert.Empty(t, f.store.calls, "无会话请求不许碰库")
			assert.Empty(t, f.homes.calls, "无会话请求不许查 home_zone")
		})
	}
}

// ---------------------------------------------------------------------------
// BrowseListings
// ---------------------------------------------------------------------------

func TestBrowseValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *tradepb.BrowseListingsRequest)
	}{
		{"tab 未指定", func(in *tradepb.BrowseListingsRequest) { in.Tab = tradepb.ListingTab_LISTING_TAB_UNSPECIFIED }},
		{"tab 未知", func(in *tradepb.BrowseListingsRequest) { in.Tab = tradepb.ListingTab(3) }},
		{"section 未指定", func(in *tradepb.BrowseListingsRequest) { in.Section = tradepb.ListingSection_LISTING_SECTION_UNSPECIFIED }},
		{"section 未知", func(in *tradepb.BrowseListingsRequest) { in.Section = tradepb.ListingSection(3) }},
		{"category 未指定", func(in *tradepb.BrowseListingsRequest) { in.Category = tradepb.ListingCategory_LISTING_CATEGORY_UNSPECIFIED }},
		{"category 越界", func(in *tradepb.BrowseListingsRequest) { in.Category = tradepb.ListingCategory(10) }},
		{"武器子类越界", func(in *tradepb.BrowseListingsRequest) { in.Subcategory = 6 }},
		{"套装不许有子类", func(in *tradepb.BrowseListingsRequest) {
			in.Category, in.Subcategory = tradepb.ListingCategory_LISTING_CATEGORY_SET, 1
		}},
		{"sort 未知", func(in *tradepb.BrowseListingsRequest) { in.Sort = tradepb.ListingSort(5) }},
		{"search 超长", func(in *tradepb.BrowseListingsRequest) { in.Search = strings.Repeat("剑", constants.MaxSearchRunes+1) }},
		{"search 含控制字符", func(in *tradepb.BrowseListingsRequest) { in.Search = "a\nb" }},
		{"search 非法 UTF-8", func(in *tradepb.BrowseListingsRequest) { in.Search = "a\xff" }},
		{"竞价分区但参数非法仍先回参数错误", func(in *tradepb.BrowseListingsRequest) {
			in.Section, in.Tab = tradepb.ListingSection_LISTING_SECTION_AUCTION, tradepb.ListingTab_LISTING_TAB_UNSPECIFIED
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			in := browseRequest()
			tc.mutate(in)

			resp, err := f.as(playerB).BrowseListings(in)

			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
			assert.Empty(t, f.store.calls)
			assert.Empty(t, f.homes.calls)
		})
	}
}

func TestBrowseAuctionDisabled(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	in := browseRequest()
	in.Section = tradepb.ListingSection_LISTING_SECTION_AUCTION

	resp, err := f.as(playerB).BrowseListings(in)

	require.NoError(t, err)
	assert.Equal(t, constants.ErrFeatureDisabled, resp.GetErrorMessage().GetId())
	assert.Empty(t, f.store.calls, "竞价分区不查库")
	assert.Empty(t, f.homes.calls, "竞价分区不查 home_zone")
}

func TestBrowseZoneScopeFiltersByCallerHomeZoneAndIgnoresZoneFilter(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	in := browseRequest()
	in.ZoneFilter = zoneB // 买家 B 在 zoneA,试图看 zoneB

	resp, err := f.as(playerB).BrowseListings(in)

	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.NotNil(t, f.store.lastQuery)
	assert.Equal(t, zoneA, f.store.lastQuery.MarketZone, "zone 范围必须按调用者 home_zone 过滤")
	assert.Equal(t, []uint64{playerB}, f.homes.calls)
	assert.Equal(t, tradepb.MarketScope_MARKET_SCOPE_ZONE, resp.GetMarketScope())
	assert.Equal(t, testNowMs, resp.GetServerNowMs())
}

func TestBrowseGlobalScopeHonoursZoneFilter(t *testing.T) {
	for _, filter := range []uint32{0, zoneB} {
		f := newFixture(t, config.ScopeGlobal)
		in := browseRequest()
		in.ZoneFilter = filter

		resp, err := f.as(playerB).BrowseListings(in)

		require.NoError(t, err)
		require.Zero(t, resp.GetErrorMessage().GetId())
		assert.Equal(t, filter, f.store.lastQuery.MarketZone)
		assert.Empty(t, f.homes.calls, "global 范围不查 home_zone")
		assert.Equal(t, tradepb.MarketScope_MARKET_SCOPE_GLOBAL, resp.GetMarketScope())
	}
}

func TestBrowseHomeZoneFailures(t *testing.T) {
	t.Run("未映射", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		resp, err := f.as(playerU).BrowseListings(browseRequest())
		require.NoError(t, err)
		assert.Equal(t, constants.ErrHomeZoneUnknown, resp.GetErrorMessage().GetId())
		assert.Empty(t, f.store.calls)
	})
	t.Run("查询故障", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.homes.err = errors.New("data_service unavailable")
		resp, err := f.as(playerB).BrowseListings(browseRequest())
		require.NoError(t, err, "故障也必须 in-band")
		assert.Equal(t, constants.ErrServiceUnavailable, resp.GetErrorMessage().GetId())
		assert.Empty(t, f.store.calls)
	})
	t.Run("未接线", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.deps.HomeZones = nil
		resp, err := f.as(playerB).BrowseListings(browseRequest())
		require.NoError(t, err)
		assert.Equal(t, constants.ErrServiceUnavailable, resp.GetErrorMessage().GetId())
	})
}

func TestBrowsePaging(t *testing.T) {
	cases := []struct {
		name                          string
		total                         uint64
		page, pageSize                uint32
		wantPage, wantSize, wantCount uint32
		wantOffset                    uint64
	}{
		{"默认页长与首页", 45, 0, 0, 1, 20, 3, 0},
		{"页长超上限钳到 20,页码超末页按末页", 45, 9999, 50, 3, 20, 3, 40},
		{"客户端显式页长 4", 9, 2, 4, 2, 4, 3, 4},
		{"空结果", 0, 5, 10, 1, 10, 1, 0},
		{"页码上限 100", 100000, 500, 20, 100, 20, 5000, 1980},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, config.ScopeGlobal)
			f.store.total = tc.total
			in := browseRequest()
			in.Page, in.PageSize = tc.page, tc.pageSize

			resp, err := f.as(playerB).BrowseListings(in)

			require.NoError(t, err)
			require.Zero(t, resp.GetErrorMessage().GetId())
			assert.Equal(t, tc.wantPage, resp.GetPage())
			assert.Equal(t, tc.wantSize, resp.GetPageSize())
			assert.Equal(t, tc.wantCount, resp.GetPageCount())
			assert.Equal(t, uint32(tc.total), resp.GetTotalCount())
			assert.Equal(t, tc.wantOffset, f.store.lastOffset)
			assert.Equal(t, uint64(tc.wantSize), f.store.lastLimit)
		})
	}
}

func TestBrowseQueryCarriesRequestAndClock(t *testing.T) {
	f := newFixture(t, config.ScopeGlobal)
	in := browseRequest()
	in.Tab = tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE
	in.Sort = tradepb.ListingSort_LISTING_SORT_REMAINING_ASC
	in.Subcategory = 3
	in.FavoritesOnly = true

	_, err := f.as(playerB).BrowseListings(in)

	require.NoError(t, err)
	q := f.store.lastQuery
	require.NotNil(t, q)
	assert.Equal(t, tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE, q.Tab)
	assert.Equal(t, tradepb.ListingSort_LISTING_SORT_REMAINING_ASC, q.Sort, "REMAINING 的具体列由 data 按 tab 选")
	assert.Equal(t, tradepb.ListingCategory_LISTING_CATEGORY_WEAPON, q.Category)
	assert.Equal(t, uint32(3), q.Subcategory)
	assert.Equal(t, testNowMs, q.NowMs)
	assert.Equal(t, playerB, q.FavoritesOf, "favorites_only 按调用者过滤")

	f2 := newFixture(t, config.ScopeGlobal)
	_, err = f2.as(playerB).BrowseListings(browseRequest())
	require.NoError(t, err)
	assert.Zero(t, f2.store.lastQuery.FavoritesOf)
}

func TestBrowseSearch(t *testing.T) {
	cases := []struct {
		search      string
		wantPattern string
		wantID      uint64
	}{
		{"", "", 0},
		{"   ", "", 0},
		{"  123  ", "%123%", 123},
		{"青锋", "%青锋%", 0},
		{"a_b%c!", "%a!_b!%c!!%", 0},
		{"0", "%0%", 0},
		{"SMK-1700000000-", "%SMK-1700000000-%", 0},
		{"99999999999999999999", "%99999999999999999999%", 0}, // 超出 uint64,只按标题
	}
	for _, tc := range cases {
		t.Run(tc.search, func(t *testing.T) {
			f := newFixture(t, config.ScopeGlobal)
			in := browseRequest()
			in.Search = tc.search

			resp, err := f.as(playerB).BrowseListings(in)

			require.NoError(t, err)
			require.Zero(t, resp.GetErrorMessage().GetId())
			assert.Equal(t, tc.wantPattern, f.store.lastQuery.TitleLikePattern)
			assert.Equal(t, tc.wantID, f.store.lastQuery.SearchListingID)
		})
	}
}

func TestBrowseBuildsSummaries(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	mine := onSaleListing(11, playerB, zoneA)
	notice := onSaleListing(12, playerA, zoneA)
	notice.NoticeEndMs = testNowMs + hourMs
	notice.SaleEndMs = testNowMs + 2*hourMs
	f.store.page = []*tradepb.TradeListingRecord{mine, notice}
	f.store.total = 2
	f.store.favorites[favoriteKey{playerB, 12}] = testNowMs

	resp, err := f.as(playerB).BrowseListings(browseRequest())

	require.NoError(t, err)
	require.Len(t, resp.GetListings(), 2)
	assert.Equal(t, []uint64{11, 12}, f.store.lastFavoriteLookup)

	first, second := resp.GetListings()[0], resp.GetListings()[1]
	assert.True(t, first.GetIsMine())
	assert.False(t, first.GetIsFavorite())
	assert.Equal(t, tradepb.ListingPhase_LISTING_PHASE_ON_SALE, first.GetPhase())
	assert.Equal(t, "青锋剑", first.GetTitle())
	assert.Equal(t, uint64(500), first.GetPriceFen())
	assert.Equal(t, zoneA, first.GetMarketZone())
	assert.Equal(t, "icon_sword", first.GetIconKey())

	assert.False(t, second.GetIsMine())
	assert.True(t, second.GetIsFavorite())
	assert.Equal(t, tradepb.ListingPhase_LISTING_PHASE_PUBLIC_NOTICE, second.GetPhase())
	assert.Equal(t, testNowMs+hourMs, second.GetNoticeEndMs())
}

func TestBrowseEmptyPageSkipsFavoriteLookup(t *testing.T) {
	f := newFixture(t, config.ScopeGlobal)

	resp, err := f.as(playerB).BrowseListings(browseRequest())

	require.NoError(t, err)
	assert.Empty(t, resp.GetListings())
	assert.False(t, f.store.called("FavoriteIDs"), "空页不查收藏")
}

func TestBrowseStoreFaultsAreInBand(t *testing.T) {
	for _, op := range []string{"CountListings", "QueryListings", "FavoriteIDs"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t, config.ScopeGlobal)
			f.store.page = []*tradepb.TradeListingRecord{onSaleListing(11, playerA, zoneA)}
			f.store.failOn[op] = errStoreDown

			resp, err := f.as(playerB).BrowseListings(browseRequest())

			require.NoError(t, err, "存储故障必须 in-band,err == nil")
			assert.Equal(t, constants.ErrServiceUnavailable, resp.GetErrorMessage().GetId())
			assert.Empty(t, resp.GetListings())
		})
	}
}

// ---------------------------------------------------------------------------
// GetListingDetail
// ---------------------------------------------------------------------------

func TestGetListingDetail(t *testing.T) {
	type expect struct {
		code        uint32
		isMine      bool
		isFavorite  bool
		homeLookups int
	}
	ended := onSaleListing(13, playerA, zoneA)
	ended.SaleEndMs = testNowMs - 1

	cases := []struct {
		name    string
		scope   string
		caller  uint64
		id      uint64
		prepare func(f *fixture)
		want    expect
	}{
		{name: "listing_id=0", scope: config.ScopeZone, caller: playerB, id: 0,
			want: expect{code: constants.ErrInvalidParameter}},
		{name: "不存在", scope: config.ScopeZone, caller: playerB, id: 999,
			want: expect{code: constants.ErrListingNotFound}},
		{name: "zone 同区买家可见", scope: config.ScopeZone, caller: playerB, id: 11,
			want: expect{homeLookups: 1}},
		{name: "zone 别区买家看不到", scope: config.ScopeZone, caller: playerC, id: 11,
			want: expect{code: constants.ErrListingNotFound, homeLookups: 1}},
		{name: "卖家本人可见且不查 home_zone", scope: config.ScopeZone, caller: playerA, id: 13,
			want: expect{isMine: true}},
		{name: "已结束对买家不可见", scope: config.ScopeGlobal, caller: playerB, id: 13,
			want: expect{code: constants.ErrListingNotFound}},
		{name: "global 别区可见且不查 home_zone", scope: config.ScopeGlobal, caller: playerC, id: 11,
			want: expect{}},
		{name: "收藏标记", scope: config.ScopeGlobal, caller: playerC, id: 11,
			prepare: func(f *fixture) { f.store.favorites[favoriteKey{playerC, 11}] = testNowMs },
			want:    expect{isFavorite: true}},
		{name: "买家 home_zone 未映射", scope: config.ScopeZone, caller: playerU, id: 11,
			want: expect{code: constants.ErrHomeZoneUnknown, homeLookups: 1}},
		{name: "home_zone 查询故障", scope: config.ScopeZone, caller: playerB, id: 11,
			prepare: func(f *fixture) { f.homes.err = errors.New("boom") },
			want:    expect{code: constants.ErrServiceUnavailable, homeLookups: 1}},
		{name: "GetListing 故障", scope: config.ScopeZone, caller: playerB, id: 11,
			prepare: func(f *fixture) { f.store.failOn["GetListing"] = errStoreDown },
			want:    expect{code: constants.ErrServiceUnavailable}},
		{name: "FavoriteExists 故障", scope: config.ScopeGlobal, caller: playerB, id: 11,
			prepare: func(f *fixture) { f.store.failOn["FavoriteExists"] = errStoreDown },
			want:    expect{code: constants.ErrServiceUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.scope)
			f.store.listings[11] = onSaleListing(11, playerA, zoneA)
			f.store.listings[13] = ended
			if tc.prepare != nil {
				tc.prepare(f)
			}

			resp, err := f.as(tc.caller).GetListingDetail(&tradepb.GetListingDetailRequest{ListingId: tc.id})

			require.NoError(t, err)
			assert.Equal(t, tc.want.code, resp.GetErrorMessage().GetId())
			assert.Len(t, f.homes.calls, tc.want.homeLookups)
			if tc.want.code != 0 {
				assert.Nil(t, resp.GetDetail(), "拒绝时不带详情")
				return
			}
			summary := resp.GetDetail().GetSummary()
			assert.Equal(t, tc.id, summary.GetListingId())
			assert.Equal(t, tc.want.isMine, summary.GetIsMine())
			assert.Equal(t, tc.want.isFavorite, summary.GetIsFavorite())
			assert.Equal(t, "详细描述", resp.GetDetail().GetDescription())
			assert.Equal(t, testNowMs, resp.GetServerNowMs())
		})
	}
}

// ---------------------------------------------------------------------------
// SetFavorite
// ---------------------------------------------------------------------------

func TestSetFavoriteCancelIsIdempotentAndSkipsListingLookup(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	f.store.favorites[favoriteKey{playerB, 11}] = testNowMs

	for i := 0; i < 2; i++ {
		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: false})
		require.NoError(t, err)
		assert.Zero(t, resp.GetErrorMessage().GetId())
		assert.False(t, resp.GetFavorite())
		assert.Equal(t, uint64(11), resp.GetListingId())
	}
	assert.Len(t, f.store.deletedFavorites, 2)
	assert.False(t, f.store.called("GetListing"), "取消收藏不查商品(已下架商品的收藏也要能删)")
	assert.Empty(t, f.homes.calls)
}

func TestSetFavoriteAdd(t *testing.T) {
	t.Run("可见商品收藏成功", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.store.listings[11] = onSaleListing(11, playerA, zoneA)

		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})

		require.NoError(t, err)
		assert.Zero(t, resp.GetErrorMessage().GetId())
		assert.True(t, resp.GetFavorite())
		require.Len(t, f.store.insertedFavorites, 1)
		assert.Equal(t, playerB, f.store.insertedFavorites[0].GetPlayerId())
		assert.Equal(t, uint64(11), f.store.insertedFavorites[0].GetListingId())
		assert.Equal(t, testNowMs, f.store.insertedFavorites[0].GetCreatedMs())
	})

	t.Run("重复收藏直接回 true,不计数不插入", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.store.listings[11] = onSaleListing(11, playerA, zoneA)
		f.store.favorites[favoriteKey{playerB, 11}] = testNowMs
		limit := uint64(maxFavoritesForTest)
		f.store.favoriteCountOverride = &limit // 即使已满,重复收藏也不该被判超限

		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})

		require.NoError(t, err)
		assert.Zero(t, resp.GetErrorMessage().GetId())
		assert.True(t, resp.GetFavorite())
		assert.False(t, f.store.called("CountFavorites"))
		assert.Empty(t, f.store.insertedFavorites)
	})

	t.Run("达到上限", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.store.listings[11] = onSaleListing(11, playerA, zoneA)
		limit := uint64(maxFavoritesForTest)
		f.store.favoriteCountOverride = &limit

		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})

		require.NoError(t, err)
		assert.Equal(t, constants.ErrFavoriteLimitReached, resp.GetErrorMessage().GetId())
		assert.False(t, resp.GetFavorite())
		assert.Empty(t, f.store.insertedFavorites)
	})

	t.Run("低于上限一条仍可收藏", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.store.listings[11] = onSaleListing(11, playerA, zoneA)
		below := uint64(maxFavoritesForTest - 1)
		f.store.favoriteCountOverride = &below

		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})

		require.NoError(t, err)
		assert.Zero(t, resp.GetErrorMessage().GetId())
		assert.Len(t, f.store.insertedFavorites, 1)
	})

	t.Run("别区商品不可收藏", func(t *testing.T) {
		f := newFixture(t, config.ScopeZone)
		f.store.listings[11] = onSaleListing(11, playerA, zoneA)

		resp, err := f.as(playerC).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})

		require.NoError(t, err)
		assert.Equal(t, constants.ErrListingNotFound, resp.GetErrorMessage().GetId())
		assert.Empty(t, f.store.insertedFavorites)
	})

	t.Run("不存在的商品", func(t *testing.T) {
		f := newFixture(t, config.ScopeGlobal)
		resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 999, Favorite: true})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrListingNotFound, resp.GetErrorMessage().GetId())
	})

	t.Run("listing_id=0", func(t *testing.T) {
		f := newFixture(t, config.ScopeGlobal)
		for _, favorite := range []bool{true, false} {
			resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{Favorite: favorite})
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
		}
		assert.Empty(t, f.store.calls)
	})
}

func TestSetFavoriteStoreFaultsAreInBand(t *testing.T) {
	cases := []struct {
		op       string
		favorite bool
	}{
		{"DeleteFavorite", false},
		{"GetListing", true},
		{"FavoriteExists", true},
		{"CountFavorites", true},
		{"InsertFavorite", true},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			f := newFixture(t, config.ScopeGlobal)
			f.store.listings[11] = onSaleListing(11, playerA, zoneA)
			f.store.failOn[tc.op] = errStoreDown

			resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: tc.favorite})

			require.NoError(t, err)
			assert.Equal(t, constants.ErrServiceUnavailable, resp.GetErrorMessage().GetId())
		})
	}
}

// ---------------------------------------------------------------------------
// GetMyShelf
// ---------------------------------------------------------------------------

func TestGetMyShelf(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	sold := onSaleListing(21, playerA, zoneA)
	sold.Status = tradepb.ListingStatus_LISTING_STATUS_SOLD
	f.store.page = []*tradepb.TradeListingRecord{onSaleListing(22, playerA, zoneA), sold}
	f.store.total = 2
	f.store.favorites[favoriteKey{playerA, 22}] = testNowMs

	resp, err := f.as(playerA).GetMyShelf(&tradepb.GetMyShelfRequest{Page: 7, PageSize: 50})

	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	assert.Equal(t, playerA, f.store.lastSeller, "货架只按调用者查")
	assert.Empty(t, f.homes.calls, "货架不查 home_zone")
	assert.Equal(t, uint32(1), resp.GetPage())
	assert.Equal(t, uint32(20), resp.GetPageSize())
	assert.Equal(t, uint32(1), resp.GetPageCount())
	assert.Equal(t, uint32(2), resp.GetTotalCount())
	assert.Equal(t, testNowMs, resp.GetServerNowMs())

	listings := resp.GetListings()
	require.Len(t, listings, 2)
	for _, s := range listings {
		assert.True(t, s.GetIsMine())
	}
	assert.True(t, listings[0].GetIsFavorite())
	assert.Equal(t, tradepb.ListingPhase_LISTING_PHASE_ENDED, listings[1].GetPhase())
}

func TestGetMyShelfStoreFaultsAreInBand(t *testing.T) {
	for _, op := range []string{"CountSellerListings", "QuerySellerListings", "FavoriteIDs"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			f.store.page = []*tradepb.TradeListingRecord{onSaleListing(22, playerA, zoneA)}
			f.store.failOn[op] = errStoreDown

			resp, err := f.as(playerA).GetMyShelf(&tradepb.GetMyShelfRequest{})

			require.NoError(t, err)
			assert.Equal(t, constants.ErrServiceUnavailable, resp.GetErrorMessage().GetId())
		})
	}
}

// 响应里不许出现卖家身份(设计 §9):钉住 ListingSummary 的字段集合。
func TestListingSummaryCarriesNoSellerIdentity(t *testing.T) {
	fields := (&tradepb.ListingSummary{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := 0; i < fields.Len(); i++ {
		names = append(names, string(fields.Get(i).Name()))
	}
	sort.Strings(names)
	for _, name := range names {
		assert.False(t, strings.Contains(name, "seller"), "ListingSummary 不应带卖家字段 %s", name)
	}
}

// ---------------------------------------------------------------------------
// 整请求业务预算(P1-9:故障必须 in-band,不能被 go-zero 服务端超时拦截器抢先回 DeadlineExceeded)
// ---------------------------------------------------------------------------

// budgetCall 是一次带 I/O 的成功请求,返回 in-band 码。
type budgetCall struct {
	name string
	call func(f *fixture) (uint32, error)
}

func budgetCalls() []budgetCall {
	return []budgetCall{
		{"BrowseListings", func(f *fixture) (uint32, error) {
			resp, err := f.as(playerB).BrowseListings(browseRequest())
			return resp.GetErrorMessage().GetId(), err
		}},
		{"GetListingDetail", func(f *fixture) (uint32, error) {
			resp, err := f.as(playerB).GetListingDetail(&tradepb.GetListingDetailRequest{ListingId: 11})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"SetFavorite", func(f *fixture) (uint32, error) {
			resp, err := f.as(playerB).SetFavorite(&tradepb.SetFavoriteRequest{ListingId: 11, Favorite: true})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"GetMyShelf", func(f *fixture) (uint32, error) {
			resp, err := f.as(playerA).GetMyShelf(&tradepb.GetMyShelfRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"SeedListing", func(f *fixture) (uint32, error) {
			resp, err := f.admin().SeedListing(validSeedRequest())
			return resp.GetErrorMessage().GetId(), err
		}},
	}
}

// 每个方法里 home_zone 查询与全部存储调用共用同一个截止时间:单次超时相加会越过服务端 Timeout,
// 只有共用一个整请求预算才能保证先于拦截器 in-band 返回。
func TestEveryMethodSharesOneRequestBudget(t *testing.T) {
	for _, tc := range budgetCalls() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			f.store.listings[11] = onSaleListing(11, playerA, zoneA)
			f.store.page = []*tradepb.TradeListingRecord{onSaleListing(11, playerA, zoneA)}
			f.store.total = 1
			budget := f.deps.Config.RequestBudget()

			before := time.Now()
			code, err := tc.call(f)
			after := time.Now()

			require.NoError(t, err)
			require.Zero(t, code)
			all := append(append([]time.Time{}, f.homes.deadlines...), f.store.deadlines...)
			require.NotEmpty(t, all, "用例必须真正走到 I/O")
			for i, deadline := range all {
				require.False(t, deadline.IsZero(), "第 %d 次 I/O 没有截止时间", i)
				assert.True(t, deadline.Equal(all[0]), "第 %d 次 I/O 的截止时间与第一次不同:串行 I/O 必须共用一个预算", i)
			}
			assert.False(t, all[0].Before(before.Add(budget)), "截止时间早于入口 + RequestBudget")
			assert.False(t, all[0].After(after.Add(budget)), "截止时间晚于出口 + RequestBudget:预算没有封顶整请求")
		})
	}
}

// 依赖不响应时,预算到期让 I/O 失败:err == nil、in-band kServiceUnavailable,且早于服务端 Timeout 返回。
func TestRequestBudgetExpiryIsInBand(t *testing.T) {
	cases := []struct {
		name    string
		blockOp string
		call    func(f *fixture) (uint32, error)
	}{
		{"浏览 COUNT 卡住", "CountListings", budgetCalls()[0].call},
		{"收藏插入卡住", "InsertFavorite", budgetCalls()[2].call},
		{"种子插入卡住", "InsertListing", budgetCalls()[4].call},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, config.ScopeGlobal)
			f.store.listings[11] = onSaleListing(11, playerA, zoneA)
			f.deps.Config.Timeout = config.MinRpcTimeoutMs // 预算 500ms,用例不必等满 3.5s
			f.store.blockOn[tc.blockOp] = true
			serverTimeout := time.Duration(f.deps.Config.Timeout) * time.Millisecond

			start := time.Now()
			code, err := tc.call(f)
			elapsed := time.Since(start)

			require.NoError(t, err, "预算到期也必须 in-band")
			assert.Equal(t, constants.ErrServiceUnavailable, code)
			assert.True(t, f.store.called(tc.blockOp), "用例必须真正卡在 %s", tc.blockOp)
			assert.True(t, elapsed < serverTimeout,
				"耗时 %v 未早于服务端 Timeout %v:in-band 结果会被 go-zero 超时拦截器丢弃", elapsed, serverTimeout)
		})
	}
}
