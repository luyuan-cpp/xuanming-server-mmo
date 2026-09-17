//go:build integration

package data

// 真库集成测试:go test -tags integration ./internal/data/...
//
// TRADE_TEST_MYSQL_DSN 为空则整组 Skip。⚠ 只能指向**一次性**测试库:用例会 DROP 并重建
// trade_listing / trade_favorite / schema_migrations。DSN 必须带库名,例如
//   appuser:apppass123@tcp(127.0.0.1:3306)/trade_it?parseTime=true&charset=utf8mb4
// 表结构先经 schemamigrate.Up 按 proto 建出,与生产同一条路径。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	tradepb "proto/trade"

	"schemamigrate"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openIntegrationDB(t *testing.T) (*sql.DB, *ListingRepo) {
	t.Helper()
	dsn := os.Getenv("TRADE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TRADE_TEST_MYSQL_DSN 未设置,跳过 trade 存储集成测试(只能指向一次性库)")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var dbName string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName))
	require.NotEmpty(t, dbName, "DSN 必须带库名")

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS trade_favorite",
		"DROP TABLE IF EXISTS trade_listing",
		"DROP TABLE IF EXISTS schema_migrations",
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{
		Database: dbName,
		Tables:   Tables(),
		Logf:     t.Logf,
	})
	require.NoError(t, err)
	require.Empty(t, report.Manual, "新建库不应出现需人工项")

	return db, NewListingRepo(db, 5*time.Second)
}

const itNow = uint64(1_800_000_000_000)

func itListing(id uint64, seller uint64, zone uint32, title string, price uint64, level uint32,
	status tradepb.ListingStatus, noticeEnd, saleEnd uint64) *tradepb.TradeListingRecord {
	return &tradepb.TradeListingRecord{
		ListingId: id, SellerPlayerId: seller, MarketZone: zone, SellerZoneAtListing: zone,
		Category: tradepb.ListingCategory_LISTING_CATEGORY_WEAPON, Subcategory: 1,
		Title: title, Level: level, PriceFen: price, Status: status,
		Summary: "summary", Description: "description of " + title, IconKey: "icon_a",
		NoticeEndMs: noticeEnd, SaleEndMs: saleEnd, CreatedMs: itNow - 1000, UpdatedMs: itNow - 1000,
	}
}

func ids(recs []*tradepb.TradeListingRecord) []uint64 {
	out := make([]uint64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.GetListingId())
	}
	return out
}

func TestListingRepoIntegration(t *testing.T) {
	_, repo := openIntegrationDB(t)
	ctx := context.Background()
	listed := tradepb.ListingStatus_LISTING_STATUS_LISTED
	locked := tradepb.ListingStatus_LISTING_STATUS_LOCKED
	sold := tradepb.ListingStatus_LISTING_STATUS_SOLD
	hour := uint64(time.Hour.Milliseconds())

	seed := []*tradepb.TradeListingRecord{
		itListing(101, 1, 1, "青锋剑", 500, 10, listed, itNow-hour, itNow+hour),       // 寄售中 zone1
		itListing(102, 1, 1, "100%_纯钢剑", 300, 30, listed, itNow-hour, itNow+2*hour), // 寄售中 zone1,标题含通配符
		itListing(103, 2, 2, "玄铁剑", 400, 20, locked, itNow-hour, itNow+hour),        // 锁定 zone2,寄售列表可见
		itListing(104, 1, 1, "公示剑", 900, 5, listed, itNow+hour, itNow+3*hour),        // 公示中
		itListing(105, 1, 1, "过期剑", 100, 1, listed, itNow-2*hour, itNow-hour),        // 已过寄售期
		itListing(106, 2, 2, "已售剑", 100, 1, sold, itNow-2*hour, itNow+hour),          // 已售
	}
	for _, rec := range seed {
		require.NoError(t, repo.InsertListing(ctx, rec))
	}
	require.Error(t, repo.InsertListing(ctx, seed[0]), "主键冲突必须报错,不许静默覆盖")

	onSale := ListingQuery{
		Tab: tradepb.ListingTab_LISTING_TAB_ON_SALE, Category: tradepb.ListingCategory_LISTING_CATEGORY_WEAPON, NowMs: itNow,
	}

	t.Run("寄售列表含 LOCKED、不含公示 / 过期 / 已售,默认新上架在前", func(t *testing.T) {
		total, err := repo.CountListings(ctx, onSale)
		require.NoError(t, err)
		assert.Equal(t, uint64(3), total)
		recs, err := repo.QueryListings(ctx, onSale, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{103, 102, 101}, ids(recs))
		assert.Empty(t, recs[0].GetDescription(), "列表查询不取 description")
		assert.Equal(t, tradepb.ListingStatus_LISTING_STATUS_LOCKED, recs[0].GetStatus())
	})

	t.Run("公示列表", func(t *testing.T) {
		q := onSale
		q.Tab = tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE
		recs, err := repo.QueryListings(ctx, q, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{104}, ids(recs))
	})

	t.Run("分区过滤 + 排序 + 分页", func(t *testing.T) {
		q := onSale
		q.MarketZone = 1
		q.Sort = tradepb.ListingSort_LISTING_SORT_PRICE_ASC
		recs, err := repo.QueryListings(ctx, q, 0, 1)
		require.NoError(t, err)
		assert.Equal(t, []uint64{102}, ids(recs))
		recs, err = repo.QueryListings(ctx, q, 1, 1)
		require.NoError(t, err)
		assert.Equal(t, []uint64{101}, ids(recs))

		q.Sort = tradepb.ListingSort_LISTING_SORT_REMAINING_ASC
		recs, err = repo.QueryListings(ctx, q, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{101, 102}, ids(recs))
	})

	t.Run("LIKE 通配符按字面量匹配", func(t *testing.T) {
		q := onSale
		q.TitleLikePattern = "%!%!_%" // 搜索词 "%_" 经 EscapeLike 后的形状
		recs, err := repo.QueryListings(ctx, q, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{102}, ids(recs), "只有标题里真含 %_ 的商品命中")
	})

	t.Run("数字搜索同时按编号", func(t *testing.T) {
		q := onSale
		q.TitleLikePattern = "%101%"
		q.SearchListingID = 101
		recs, err := repo.QueryListings(ctx, q, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{101}, ids(recs))
	})

	t.Run("收藏:幂等插入、批量查、只看收藏、删除", func(t *testing.T) {
		fav := &tradepb.TradeFavoriteRecord{PlayerId: 9, ListingId: 101, CreatedMs: itNow}
		require.NoError(t, repo.InsertFavorite(ctx, fav))
		require.NoError(t, repo.InsertFavorite(ctx, fav), "重复收藏必须幂等")

		n, err := repo.CountFavorites(ctx, 9)
		require.NoError(t, err)
		assert.Equal(t, uint64(1), n)

		exists, err := repo.FavoriteExists(ctx, 9, 101)
		require.NoError(t, err)
		assert.True(t, exists)

		set, err := repo.FavoriteIDs(ctx, 9, []uint64{101, 102})
		require.NoError(t, err)
		assert.Equal(t, map[uint64]bool{101: true}, set)

		q := onSale
		q.FavoritesOf = 9
		recs, err := repo.QueryListings(ctx, q, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{101}, ids(recs))

		require.NoError(t, repo.DeleteFavorite(ctx, 9, 101))
		require.NoError(t, repo.DeleteFavorite(ctx, 9, 101), "取消收藏必须幂等")
		exists, err = repo.FavoriteExists(ctx, 9, 101)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("货架含任意状态,新上架在前", func(t *testing.T) {
		total, err := repo.CountSellerListings(ctx, 2)
		require.NoError(t, err)
		assert.Equal(t, uint64(2), total)
		recs, err := repo.QuerySellerListings(ctx, 2, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, []uint64{106, 103}, ids(recs))
	})

	t.Run("详情取全列;不存在返回 ErrListingNotFound", func(t *testing.T) {
		rec, err := repo.GetListing(ctx, 102)
		require.NoError(t, err)
		assert.Equal(t, "100%_纯钢剑", rec.GetTitle())
		assert.Equal(t, "description of 100%_纯钢剑", rec.GetDescription())
		assert.Equal(t, tradepb.ListingCategory_LISTING_CATEGORY_WEAPON, rec.GetCategory())

		_, err = repo.GetListing(ctx, 999)
		assert.True(t, errors.Is(err, ErrListingNotFound))
	})
}
