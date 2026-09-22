//go:build integration

package data

// 真库集成测试:go test -tags integration ./internal/data/...
//
// TRADE_TEST_MYSQL_DSN 为空则整组 Skip。⚠ 只能指向**一次性**测试库:用例会 DROP 并重建
// trade_listing / trade_favorite / schema_migrations。DSN 必须带库名,例如
//   appuser:apppass123@tcp(127.0.0.1:3306)/trade_it?parseTime=true&charset=utf8mb4
// 表结构先经 schemamigrate.Up 按 proto 建出,与生产同一条路径。
//
// 收藏锁模式的三条用例(文件末尾,审计 #9)要靠 performance_schema.data_lock_waits 观察锁等待来编排时序,
// 测试账号没有它的 SELECT 权限时(非验收模式下)会 Skip —— **Skip 不代表通过**。
//
// # 验收开关 TRADE_REQUIRE_MYSQL_TESTS(与 go/friend 的 FRIEND_REQUIRE_MYSQL_TESTS 同一口径)
//
// 它**不是**第二个门控(门控仍只有 TRADE_TEST_MYSQL_DSN),只决定"跳过算不算失败"。设了之后:
//   - TRADE_TEST_MYSQL_DSN 为空 → 判红,不再整组静默 Skip;
//   - 读不了 performance_schema 的锁视图 → 确定性用例判红,压力用例也直接 Fatal,不再退化为"不确认排队"的纯并发。
//
// 验收一律这样跑(PowerShell,工作目录 go/trade;账号必须能 SELECT performance_schema.data_locks /
// data_lock_waits,库级授权的 appuser 通常不行,用一次性库上的 root;密码见 deploy/docker-compose.yml,不要写进任何被跟踪的文件):
//
//   $env:TRADE_TEST_MYSQL_DSN='root:<root 密码>@tcp(127.0.0.1:3306)/trade_it?parseTime=true&charset=utf8mb4'
//   $env:TRADE_REQUIRE_MYSQL_TESTS='1'
//   go test -tags integration ./internal/data/ -count=1 -v
//
// ⚠ 不带 -tags integration 时本文件整个不参与编译,这个开关也就无从生效,go test 照样输出 ok ——
// 验收命令必须带标签,并在 -v 输出里逐条看到本文件的用例名与 PASS。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tradepb "proto/trade"

	"schemamigrate"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tradeRequireMySQLEnv 是验收开关(见文件头注):设了就把"跳过"一律判红。
// 为什么要它:friend 的先例(go/friend/internal/data/friend_guard_lock_order_mysql_test.go 头注)里,一次 1213
// 被盖了一个多月,就是因为"全体 Skip"在报告里与"全绿"长得一样;不加 -v 的 go test 对 Skip 同样只输出 ok。
const tradeRequireMySQLEnv = "TRADE_REQUIRE_MYSQL_TESTS"

// requireMySQLTests 报告是否处于验收模式。
func requireMySQLTests() bool {
	return os.Getenv(tradeRequireMySQLEnv) != ""
}

func openIntegrationDB(t *testing.T) (*sql.DB, *ListingRepo) {
	t.Helper()
	dsn := os.Getenv("TRADE_TEST_MYSQL_DSN")
	if dsn == "" {
		if requireMySQLTests() {
			t.Fatalf("%s 已设置(验收模式),但 TRADE_TEST_MYSQL_DSN 为空:本文件的真库用例会整组静默 Skip", tradeRequireMySQLEnv)
		}
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
		itListing(101, 1, 1, "青锋剑", 500, 10, listed, itNow-hour, itNow+hour),        // 寄售中 zone1
		itListing(102, 1, 1, "100%_纯钢剑", 300, 30, listed, itNow-hour, itNow+2*hour), // 寄售中 zone1,标题含通配符
		itListing(103, 2, 2, "玄铁剑", 400, 20, locked, itNow-hour, itNow+hour),        // 锁定 zone2,寄售列表可见
		itListing(104, 1, 1, "公示剑", 900, 5, listed, itNow+hour, itNow+3*hour),       // 公示中
		itListing(105, 1, 1, "过期剑", 100, 1, listed, itNow-2*hour, itNow-hour),       // 已过寄售期
		itListing(106, 2, 2, "已售剑", 100, 1, sold, itNow-2*hour, itNow+hour),         // 已售
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

// ── 收藏写入的锁模式回归(2026-09-21 死锁审计 #9)────────────────────────────
//
// # 成环的真实前提
//
// 环不是"两个收藏并发"就能出现的:单独一条 INSERT 在重复键检查里取到 S、判定"记录只是删除标记"、再申请 X 复活它,
// 这几步在同一个 mini-transaction 的页闩下连续完成(trade_favorite 全是定长列,复活走原地更新,不会中途放闩重来),
// 别的事务插不进来拿 S。所以"T1 执行完 → T2 发出 → T1 提交"这种两方编排,旧的 INSERT IGNORE 也**不会**死锁:
// T2 只能排在 T1 的 X 后面,T1 提交后 T2 看见活行、按重复忽略。拿它当红对照,红永远不红,绿也就什么都没证明
// (审计 #9 原稿"两边都执行完第一条语句再提交"同理不成立:第二条语句在第一方提交前根本执行不完)。
//
// 真正的前提是 MySQL 手册 "Locks Set by Different SQL Statements" 三会话例的形状:**有第三方正持着这条删除标记
// 记录的 X,至少两个收藏语句同时排在它后面**。本调用路径上的第三方只有两种(favoriteLockHolder):
//   - 取消收藏(DeleteFavorite)的 DELETE 删了活行、尚未提交(自动提交语句从取锁到提交之间同样持着 X);
//   - 先到的收藏已经把删除标记记录复活、随后回滚(ctx 超时、连接断开、被选为死锁牺牲者)。
//
// 第三方一放锁:
//   - INSERT IGNORE:重复键检查取 **S**,两个排队者**同时**拿到(S 与 S 相容);记录仍是已提交的删除标记 → 不算重复
//     → 复活它要 **X** → 各被对方的 S 挡住 → InnoDB 牺牲其一(1213)。
//   - insertFavoriteSQL(ODKU):重复键检查直接取 **X**,只有一个排队者拿到;它复活记录,另一个排到它提交之后看见活行,
//     走 no-op 更新。不存在升级,成不了环。
//
// # 编排
//
// 与 go/friend 场景 (g)(h) 同一套办法:靠 performance_schema **观察**两个排队者真的进了锁等待队列再放锁,
// 不靠并发度去撞、不靠 sleep 估时间。红绿两组用例走**同一个**编排函数,唯一的变量是收藏语句本身。
//
// purge 挡板:一个 REPEATABLE READ 事务在删除提交**之前**做一次一致性读,它的 read view 让 purge 不能物理清掉
// 这条删除标记记录。不挡的话,purge 抢在排队者重跑之前删掉记录时,锁会被继承成间隙锁,形状就变成 InsertFavorite
// 注释里那类"同键并发插入 + purge"的固有情形(由 WithTxRetry 的有界重试兜住),结论不再确定。
//
// (InnoDB 的取锁细节是按手册与 row_ins_duplicate_error_in_clust 的已知行为推演的,以真库上跑出来的结果为准;
// 红对照若在某个 MySQL 版本上复现不稳定,按 friend 场景 (h) 的办法处理:注明版本、降级为不强制,而不是删掉。)

// legacyInsertIgnoreFavoriteSQL 是 2026-09-21 之前 InsertFavorite 用的语句。生产代码里已经没有它;这里留一份
// **只**给红对照用:证明编排确实走到了会成环的形状,绿用例的"不死锁"才有证明力。
const legacyInsertIgnoreFavoriteSQL = "INSERT IGNORE INTO trade_favorite (`player_id`, `listing_id`, `created_ms`) VALUES (?, ?, ?)"

// favoriteDeleteSQL 与 DeleteFavorite 的语句逐字一致(生产代码里是内联字面量)。DeleteFavorite 自动提交,停不在提交之前,
// 所以扮演"未提交的取消收藏"时只能把同一条语句放进显式事务;主键等值删除的锁行为与语句所在的事务形态无关。
const favoriteDeleteSQL = "DELETE FROM trade_favorite WHERE `player_id` = ? AND `listing_id` = ?"

const (
	// favoriteRaceBudget 是一次编排的总时间预算:集成测试不许无界等待,真卡住时在这里超时判红,而不是挂住 go test。
	favoriteRaceBudget = 30 * time.Second
	// favoriteWaitBudget 是每一步"等某件事发生"的上限(等待者到齐、语句返回、清理)。正常情况下都是毫秒级。
	favoriteWaitBudget = 10 * time.Second

	favoriteTestListing uint64 = 9_200_001
	favoriteSeedMs      uint64 = 1   // 夹具先插入再删除的那一版
	favoriteHolderMs    uint64 = 100 // "先到后回滚"的收藏写入、随后被回滚掉的值
	favoriteWaiterMs    uint64 = 201 // 两个排队者依次取 201、202,终态 created_ms 能指认是谁写的
)

// favoriteLockHolder 是排在两个收藏语句前面、持有那条记录 X 锁的第三方。
type favoriteLockHolder int

const (
	// holderPendingUnfavorite:取消收藏的 DELETE 已删掉活行、尚未提交。放锁 = 提交,留下已提交的删除标记。
	holderPendingUnfavorite favoriteLockHolder = iota
	// holderRolledBackFavorite:先到的收藏已把已提交的删除标记记录复活、随后回滚。放锁 = 回滚,记录退回删除标记。
	holderRolledBackFavorite
)

// favoriteLockHolderCases 是红绿两组共用的现场;每个现场用不同的玩家,互不干扰。
var favoriteLockHolderCases = []struct {
	name   string
	holder favoriteLockHolder
	player uint64
}{
	{"取消收藏的DELETE未提交", holderPendingUnfavorite, 9_100_001},
	{"先到的收藏复活记录后回滚", holderRolledBackFavorite, 9_100_002},
}

// favoriteStmt 是在独立 READ COMMITTED 事务里执行的一条收藏语句(与 InsertFavorite 经 WithTxRetry 开的事务同一隔离级)。
type favoriteStmt struct {
	name      string
	createdMs uint64
	tx        *sql.Tx
	finished  chan struct{} // 语句返回(成功或失败)后关闭;关闭之后才能读 err
	err       error
}

func startFavoriteStmt(t *testing.T, ctx context.Context, db *sql.DB, stmt, name string, player, listing, createdMs uint64) *favoriteStmt {
	t.Helper()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err, "夹具:开 %s 的事务失败", name)
	s := &favoriteStmt{name: name, createdMs: createdMs, tx: tx, finished: make(chan struct{})}
	go func() {
		defer close(s.finished)
		_, s.err = tx.ExecContext(ctx, stmt, player, listing, createdMs)
	}()
	return s
}

// waitFavoriteStmt 等 s 的语句返回(之后才能读 s.err),最多等 favoriteWaitBudget。
func waitFavoriteStmt(t *testing.T, s *favoriteStmt) {
	t.Helper()
	select {
	case <-s.finished:
	case <-time.After(favoriteWaitBudget):
		t.Fatalf("%s 在 %v 内没有返回:持锁方已经放锁,它仍卡着 —— 锁行为与本用例的推演不符", s.name, favoriteWaitBudget)
	}
}

// startDeleteMarkedFavoriteRace 把现场摆到"持锁方刚放锁"的那一刻,返回两个排队的收藏语句,结局由调用方判定:
//  1. 开 purge 挡板;
//  2. 按 holder 造出持 X 的第三方,(player, listing) 此时是删除标记记录;
//  3. 起两个收藏语句,**确认**两者都排进了这张表的锁等待队列;
//  4. 放锁(DELETE 提交 / 复活回滚)。
//
// 编排失败一律 Fatal(唯一例外:读不了 performance_schema 且不在验收模式时 Skip),不会返回半成品现场。清理挂在 t.Cleanup 上,
// 成功、失败、Skip 三条路径都会走到。
func startDeleteMarkedFavoriteRace(t *testing.T, db *sql.DB, stmt string, holder favoriteLockHolder, player, listing uint64) (context.Context, [2]*favoriteStmt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), favoriteRaceBudget)
	var (
		purgeTx, holderTx *sql.Tx
		waiters           [2]*favoriteStmt
	)
	t.Cleanup(func() {
		// 顺序:先取消 ctx → 等排队者的 goroutine 退出、显式回滚剩下的事务(已结束的再回滚无害)→ 最后删本用例的行。
		//
		// 取消 ctx 的效果要说准:go-sql-driver 在 ctx 取消时只关掉本地 socket,**不**向服务端发 KILL QUERY。
		//   - 语句仍在途的排队者:客户端调用立刻返回,但服务端那条语句照旧留在锁等待里,直到锁被授予(或
		//     innodb_lock_wait_timeout 到期)、执行完、下一次读写 socket 发现断连,服务端线程才退出并回滚它的事务;
		//   - 连接空闲的事务(持锁方、purge 挡板、语句已返回的排队者):database/sql 在 ctx 取消时经仍然完好的连接
		//     发出真正的 ROLLBACK,锁当场释放。
		// 本编排里排队者只会等持锁方或另一个排队者的锁:前者随 ROLLBACK 当场释放,后者一拿到锁就执行完、随即发现断连
		// 回滚,所以这些孤儿语句按毫秒级依次结束。最后那条清理 DELETE 可能短暂排在它们后面,由 cleanupFavoriteRow 的
		// favoriteWaitBudget 兜底,超时 t.Errorf 判红,不会静默挂住。
		//
		// 刻意不在排队者事务里记 CONNECTION_ID()、清理时 KILL:mysql 驱动支持会话重置,database/sql 回滚后会把连接
		// **放回连接池**。排队者的语句若恰在取消前返回,记下的 id 此刻可能已属于池里别的活连接(甚至正是这条清理
		// DELETE 用的那条),KILL 会误伤;要做安全得给每个排队者固定一条 *sql.Conn 再加一套时序,换来的只是
		// 失败路径上毫秒级的收敛提速,不值得。
		cancel()
		for _, w := range waiters {
			if w == nil {
				continue
			}
			select {
			case <-w.finished:
				_ = w.tx.Rollback()
			case <-time.After(favoriteWaitBudget):
				t.Errorf("清理:%s 在取消 ctx 之后 %v 内仍未返回", w.name, favoriteWaitBudget)
			}
		}
		for _, tx := range []*sql.Tx{holderTx, purgeTx} {
			if tx != nil {
				_ = tx.Rollback()
			}
		}
		cleanupFavoriteRow(t, db, player, listing)
	})

	// 1. purge 挡板必须在任何删除提交**之前**建好 read view。
	purgeTx = openPurgeBlocker(t, ctx, db, player)

	// 2. 持锁方。
	mustAffectOneRow(t, ctx, db, "夹具:插入活行",
		"INSERT INTO trade_favorite (`player_id`, `listing_id`, `created_ms`) VALUES (?, ?, ?)", player, listing, favoriteSeedMs)
	var err error
	switch holder {
	case holderPendingUnfavorite:
		holderTx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err, "夹具:开取消收藏事务失败")
		mustAffectOneRow(t, ctx, holderTx, "夹具:未提交的取消收藏应当删掉活行(此时持有该主键的 X)",
			favoriteDeleteSQL, player, listing)
	case holderRolledBackFavorite:
		mustAffectOneRow(t, ctx, db, "夹具:删除活行、留下已提交的删除标记", favoriteDeleteSQL, player, listing)
		holderTx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err, "夹具:开先到收藏的事务失败")
		// 影响 1 行 = 真的走了"复活删除标记记录"的插入分支;记录若还是活行,INSERT IGNORE 影响 0 行且不持 X,编排就不成立。
		mustAffectOneRow(t, ctx, holderTx, "夹具:先到的收藏应当复活删除标记记录",
			stmt, player, listing, favoriteHolderMs)
	default:
		t.Fatalf("未知的持锁方 %d", holder)
	}

	// 3. 两个排队者。
	for i := range waiters {
		waiters[i] = startFavoriteStmt(t, ctx, db, stmt, fmt.Sprintf("收藏#%d", i+1), player, listing, favoriteWaiterMs+uint64(i))
	}
	if _, err := awaitLockWaiters(ctx, db, "trade_favorite", len(waiters), favoriteWaitBudget); err != nil {
		failOrSkipOnLockWaitError(t, err, "持锁方未放锁,两个收藏语句都应当卡在这条记录的重复键检查上")
	}

	// 4. 放锁:旧语句成环的时刻就在这里。
	if holder == holderPendingUnfavorite {
		err = holderTx.Commit()
	} else {
		err = holderTx.Rollback()
	}
	require.NoError(t, err, "夹具:持锁方放锁失败")
	return ctx, waiters
}

// TestLegacyInsertIgnoreFavoriteDeadlocksOnDeleteMarkedRow 是**红对照**:旧的 INSERT IGNORE 在同一编排下必须复现 1213,
// 且恰好一个排队者被牺牲、另一个成功。它不验产品代码,验的是"编排确实走到了成环形状";它不红的时候,
// 绿用例的"不死锁"就什么都没证明。
func TestLegacyInsertIgnoreFavoriteDeadlocksOnDeleteMarkedRow(t *testing.T) {
	db, _ := openIntegrationDB(t)
	for _, tc := range favoriteLockHolderCases {
		t.Run(tc.name, func(t *testing.T) {
			_, waiters := startDeleteMarkedFavoriteRace(t, db, legacyInsertIgnoreFavoriteSQL, tc.holder, tc.player, favoriteTestListing)

			var deadlocked, succeeded int
			for _, w := range waiters {
				waitFavoriteStmt(t, w)
				switch {
				case isInnoDBDeadlock(w.err):
					deadlocked++
				case w.err == nil:
					succeeded++
				default:
					t.Fatalf("%s 出现非预期错误(既不是成功也不是 1213): %v", w.name, w.err)
				}
			}
			if deadlocked != 1 || succeeded != 1 {
				t.Fatalf("红对照没有复现死锁:期望恰好 1 个 1213、1 个成功,got 1213=%d 成功=%d。"+
					"说明放锁时两个 S 没有被同时授予(或记录已被 purge),编排没走到成环形状,"+
					"TestInsertFavoriteSQLRevivesDeleteMarkedRowWithoutDeadlock 在这个 MySQL 版本上失去证明力(见本节头注)",
					deadlocked, succeeded)
			}
		})
	}
}

// TestInsertFavoriteSQLRevivesDeleteMarkedRowWithoutDeadlock 是**绿**:同一编排换成生产常量 insertFavoriteSQL,
// 两个排队者都必须成功、终态恰好 1 行、created_ms 是先完成者写入的值(后到者走 no-op 更新,不刷新收藏时间)。
//
// 放锁之后谁先拿到 X 由 InnoDB 决定(CATS 按事务权重,不按到达顺序),所以这里**观察**谁先返回,而不是预设:
// 先完成者返回 → 确认另一个仍在锁等待队列里 → 提交先完成者 → 另一个才返回。
func TestInsertFavoriteSQLRevivesDeleteMarkedRowWithoutDeadlock(t *testing.T) {
	db, _ := openIntegrationDB(t)
	for _, tc := range favoriteLockHolderCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, waiters := startDeleteMarkedFavoriteRace(t, db, insertFavoriteSQL, tc.holder, tc.player, favoriteTestListing)

			var first, second *favoriteStmt
			select {
			case <-waiters[0].finished:
				first, second = waiters[0], waiters[1]
			case <-waiters[1].finished:
				first, second = waiters[1], waiters[0]
			case <-time.After(favoriteWaitBudget):
				t.Fatalf("持锁方放锁后 %v 内两个收藏语句都没有返回", favoriteWaitBudget)
			}
			requireNoFavoriteDeadlock(t, first)

			// 先完成者持着 X 未提交,另一个此刻必须还在排队:ODKU 的重复键检查取 X,不可能与它并行。
			select {
			case <-second.finished:
				t.Fatalf("%s 在 %s 提交之前就返回了(err=%v):重复键检查没有取 X —— insertFavoriteSQL 被改成别的写法了?",
					second.name, first.name, second.err)
			default:
			}
			if _, err := awaitLockWaiters(ctx, db, "trade_favorite", 1, favoriteWaitBudget); err != nil {
				failOrSkipOnLockWaitError(t, err, second.name+" 应当排在先完成者的 X 后面")
			}

			require.NoError(t, first.tx.Commit(), "提交 %s 失败", first.name)
			waitFavoriteStmt(t, second)
			requireNoFavoriteDeadlock(t, second)
			require.NoError(t, second.tx.Commit(), "提交 %s 失败", second.name)

			assert.Equal(t, first.createdMs, readOnlyFavoriteCreatedMs(t, ctx, db, tc.player, favoriteTestListing),
				"created_ms 必须是先完成者 %s 写入的值:后到者只能走 no-op 更新,不许覆盖", first.name)
		})
	}
}

// TestInsertFavoriteConcurrentRevivalStress 是压力式补充:走完整的产品路径 repo.InsertFavorite(WithTxRetry 的 RC 事务、
// 连接池、有界重试),每轮让 N 个并发收藏排在一条未提交的取消收藏后面,放锁后断言零错误、终态恰好 1 行、
// 且这一行是本轮复活出来的。
//
// 与上面确定性用例的分工:repo.InsertFavorite 会用重试吸收一次 1213,所以本用例**不能**证明"一次 1213 都没有" ——
// 那是绿用例的职责;本用例证明的是产品路径在高并发、多轮删除标记复活下对调用方零错误。
// 读不了 performance_schema 时:非验收模式退化为"不确认排队"的纯并发轮次(照样有价值,只是未必撞到最坏形状),不 Skip;
// 验收模式(TRADE_REQUIRE_MYSQL_TESTS)下直接 Fatal —— 验收要的是"确认排队之后仍零错误",退化轮次替代不了。
func TestInsertFavoriteConcurrentRevivalStress(t *testing.T) {
	db, repo := openIntegrationDB(t)
	const (
		player  uint64 = 9_100_101
		listing        = favoriteTestListing
		workers        = 8
		rounds         = 200
	)
	// 每轮 workers 个收藏 + 持锁方 + 观察查询同时占连接;空闲池太小的话每轮都在重新建连,拖慢且与被测行为无关。
	db.SetMaxIdleConns(workers + 4)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	var purgeTx *sql.Tx
	t.Cleanup(func() {
		// 先取消 ctx,再回滚 purge 挡板,最后删本用例的行。取消的效果同 startDeleteMarkedFavoriteRace 的清理注释:
		// 驱动只关本地 socket、不发 KILL QUERY。仍开着的持锁方事务(连接空闲)由 database/sql 经完好的连接发 ROLLBACK,
		// 锁当场释放;各轮的收藏 goroutine 在任何 Fatal 之前都已 wg.Wait 收齐,但其中因 opTimeout / 用例 ctx 到期先在客户端返回的,
		// 服务端语句可能还在排队 —— 它们只等持锁方或同轮其它收藏,前者已释放、后者执行完即发现断连回滚,毫秒级收敛;
		// 清理 DELETE 由 favoriteWaitBudget 兜底,超时判红。
		cancel()
		if purgeTx != nil {
			_ = purgeTx.Rollback()
		}
		cleanupFavoriteRow(t, db, player, listing)
	})
	purgeTx = openPurgeBlocker(t, ctx, db, player)
	require.NoError(t, repo.InsertFavorite(ctx, &tradepb.TradeFavoriteRecord{PlayerId: player, ListingId: listing, CreatedMs: favoriteSeedMs}))

	observeWaiters := true
	for round := 0; round < rounds; round++ {
		holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err, "第 %d 轮:开取消收藏事务失败", round)
		mustAffectOneRow(t, ctx, holder, fmt.Sprintf("第 %d 轮:取消收藏应当删掉上一轮留下的那 1 行", round),
			favoriteDeleteSQL, player, listing)

		base := uint64(round+1) * 1000
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = repo.InsertFavorite(ctx, &tradepb.TradeFavoriteRecord{PlayerId: player, ListingId: listing, CreatedMs: base + uint64(i)})
			}(i)
		}
		close(start)
		if observeWaiters {
			if _, err := awaitLockWaiters(ctx, db, "trade_favorite", workers, favoriteWaitBudget); err != nil {
				unobservable := errors.Is(err, errLockWaitsUnobservable)
				if !unobservable || requireMySQLTests() {
					// 先放锁再收齐 goroutine:不放锁的话它们要一直等到 opTimeout,Fatal 之后还留一串孤儿语句。
					_ = holder.Rollback()
					wg.Wait()
					if unobservable {
						t.Fatalf("第 %d 轮:%s 已设置(验收模式),不许退化为不确认排队的纯并发"+
							"(测试账号需要 performance_schema 的 SELECT 权限): %v", round, tradeRequireMySQLEnv, err)
					}
					t.Fatalf("第 %d 轮:%v —— 未提交的取消收藏应当让全部 %d 个收藏卡在重复键检查上", round, err, workers)
				}
				observeWaiters = false
				t.Logf("读不了 performance_schema,之后各轮不再确认排队,退化为纯并发(不代表撞到了最坏形状): %v", err)
			}
		}
		commitErr := holder.Commit()
		wg.Wait() // 每个 InsertFavorite 都受 repo 的 opTimeout 约束,不会无界等待
		require.NoError(t, commitErr, "第 %d 轮:提交取消收藏失败", round)

		for i, err := range errs {
			if isInnoDBDeadlock(err) {
				t.Fatalf("第 %d 轮收藏 #%d 返回 1213,连重试上限都用尽了:收藏写入又在删除标记记录上 S→X 互等"+
					"(insertFavoriteSQL 被改回 INSERT IGNORE?见本节头注)—— %v", round, i, err)
			}
			require.NoError(t, err, "第 %d 轮收藏 #%d", round, i)
		}
		created := readOnlyFavoriteCreatedMs(t, ctx, db, player, listing)
		if created < base || created >= base+workers {
			t.Fatalf("第 %d 轮:created_ms=%d 不在本轮写入值 [%d, %d) 内:这一行不是本轮复活出来的", round, created, base, base+workers)
		}
	}
}

// openPurgeBlocker 开一个 REPEATABLE READ 只读事务并做一次一致性读:此刻建好的 read view 让 purge 不能物理清掉
// 之后才提交的删除标记记录,直到本事务结束。调用方负责在清理时回滚它。
// 用带 WHERE 的普通查询,它一定走 row_search_mvcc、一定分配 read view;不用无条件 COUNT(*),免得落到别的计数路径上。
func openPurgeBlocker(t *testing.T, ctx context.Context, db *sql.DB, player uint64) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err, "夹具:开 purge 挡板事务失败")
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM trade_favorite WHERE `player_id` = ?", player).Scan(&n); err != nil {
		_ = tx.Rollback()
		t.Fatalf("夹具:purge 挡板的一致性读失败: %v", err)
	}
	return tx
}

// readOnlyFavoriteCreatedMs 断言该玩家恰好只有 (player, listing) 这一条收藏,返回它的 created_ms。
func readOnlyFavoriteCreatedMs(t *testing.T, ctx context.Context, db *sql.DB, player, listing uint64) uint64 {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM trade_favorite WHERE `player_id` = ?", player).Scan(&n))
	require.Equal(t, 1, n, "终态该玩家必须恰好 1 条收藏")
	var created uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT `created_ms` FROM trade_favorite WHERE `player_id` = ? AND `listing_id` = ?", player, listing).Scan(&created))
	return created
}

// cleanupFavoriteRow 删掉本用例造的收藏行(AGENTS.md §11.4:集成测试清理自身状态)。用独立的有界 ctx:
// 走到这里时用例自己的 ctx 往往已经取消。
func cleanupFavoriteRow(t *testing.T, db *sql.DB, player, listing uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), favoriteWaitBudget)
	defer cancel()
	if _, err := db.ExecContext(ctx, favoriteDeleteSQL, player, listing); err != nil {
		t.Errorf("清理:删除本用例的收藏行失败: %v", err)
	}
}

// favoriteExecer 是 *sql.DB 与 *sql.Tx 的公共 ExecContext,让夹具语句在事务内外共用一个断言。
type favoriteExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// mustAffectOneRow 执行 query 并断言恰好影响 1 行。
func mustAffectOneRow(t *testing.T, ctx context.Context, ex favoriteExecer, what, query string, args ...any) {
	t.Helper()
	res, err := ex.ExecContext(ctx, query, args...)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	if err != nil || n != 1 {
		t.Fatalf("%s:期望恰好影响 1 行,got affected=%d err=%v", what, n, err)
	}
}

func requireNoFavoriteDeadlock(t *testing.T, s *favoriteStmt) {
	t.Helper()
	if isInnoDBDeadlock(s.err) {
		t.Fatalf("%s 返回 InnoDB 死锁(1213):收藏写入在删除标记记录上 S→X 升级、与另一个排队者互等"+
			"(insertFavoriteSQL 被改回 INSERT IGNORE 了?见本节头注)—— %v", s.name, s.err)
	}
	require.NoError(t, s.err, "%s 出现非预期错误", s.name)
}

// isInnoDBDeadlock 只认 1213。刻意不用 IsRetryableTxError:它把 1205 锁等待超时也算进来,
// 而本组用例里 1205 意味着"卡住了",与"成环了"是两种结论,不能混在一起。
func isInnoDBDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213
	}
	// 兜底:中间某层只保留了 Error() 文本、errors.As 链断掉时。
	return strings.Contains(err.Error(), "Error 1213")
}

// lockWaitersQuery 数本库里正在排队等某张表上行锁的事务数(与 go/friend 场景 (g)(h) 同一口径)。
// 只看当前库、指定表:同一个 MySQL 实例上别的库、别的表的锁等待不算数。
const lockWaitersQuery = `
	SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
	FROM performance_schema.data_lock_waits w
	JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
	WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?`

// errLockWaitsUnobservable:测试账号读不了 performance_schema 的锁视图,或实例上根本没有这两张表。
// 这时手工编排做不出来 —— 那不是产品缺陷,由 failOrSkipOnLockWaitError 按验收模式决定跳过还是判红。
// 只有 isLockWaitsUnobservable 认定的错误号才归到这里,见 lockWaitsQueryError。
var errLockWaitsUnobservable = errors.New("读 performance_schema.data_lock_waits 失败")

// awaitLockWaiters 轮询,直到 table 上至少有 want 个事务在排队等行锁,或 budget 用尽(返回错误)。
// 靠轮询**观察**,不靠 sleep 估时间:两次轮询之间的短停顿只是不去空转打爆 MySQL,
// 条件不满足就一直等到预算用尽并报错,不会"睡够了就当它们已经在等"。
// 查询失败时的定性见 lockWaitsQueryError:只有权限 / 对象缺失类错误算"不可观测",ctx 结束与其余错误都判红。
func awaitLockWaiters(ctx context.Context, db *sql.DB, table string, want int, budget time.Duration) (int, error) {
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := db.QueryRowContext(ctx, lockWaitersQuery, table).Scan(&waiters); err != nil {
			return 0, lockWaitsQueryError(ctx, err)
		}
		if waiters >= want {
			return waiters, nil
		}
		if time.Now().After(deadline) {
			return waiters, fmt.Errorf("%v 内只看到 %d/%d 个事务排进 %s 的锁等待队列", budget, waiters, want, table)
		}
		time.Sleep(pollEvery)
	}
}

// lockWaitsQueryError 给 lockWaitersQuery 的失败定性:是"环境看不见锁等待"(可按模式跳过),还是"编排出错"(判红)。
//   - 用例 ctx 已结束(预算用尽 / 被取消):带出 ctx 错误(errors.Is 可认出 context.DeadlineExceeded / Canceled),判红。
//     这时查询失败只是结果,原因是编排卡住或超了预算。先看 ctx.Err() 而不是先看错误本身,是因为 ctx 结束时驱动
//     报出来的形态不固定(可能是 ctx 错误,也可能是 invalid connection 之类),以 ctx 的状态为准。
//   - MySQL 权限 / 对象缺失类错误(isLockWaitsUnobservable):归为 errLockWaitsUnobservable。
//   - 其余一律判红:断连、SQL 写错、服务端内部错误都说明编排本身坏了。若也当成"不可观测",
//     卡住就会被报成 SKIP —— 该红的时候不红。
//
// 与 go/friend/internal/data/friend_guard_lock_order_mysql_test.go 的同名助手同一口径(代码只差驱动包的导入别名),
// 改一处要同步另一处。
func lockWaitsQueryError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("轮询锁等待时用例 ctx 已结束(编排卡住或超出预算,不是读不了 performance_schema): %w; 查询错误: %v", ctxErr, err)
	}
	if isLockWaitsUnobservable(err) {
		return fmt.Errorf("%w: %v", errLockWaitsUnobservable, err)
	}
	return fmt.Errorf("查询 performance_schema 锁等待失败,且不是权限 / 对象缺失类错误,按编排失败判红: %w", err)
}

// isLockWaitsUnobservable 只按驱动给出的 MySQL 错误号判定"账号或实例不支持观察锁等待" —— 都是环境问题,
// 与被测的锁行为无关。刻意不匹配错误文本,也不把"查询失败"整体归进来。
// 注意 performance_schema=OFF 时表仍在、只是恒为空,查询不报错,会走到 awaitLockWaiters 的"等待者没到齐"而判红;
// 本仓的 MySQL 8 默认开启,真遇到时先检查 SELECT @@performance_schema。
func isLockWaitsUnobservable(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1044, // ER_DBACCESS_DENIED_ERROR:对 performance_schema 库整体无权
		1142, // ER_TABLEACCESS_DENIED_ERROR:对 data_lock_waits / data_locks 没有 SELECT 权限
		1143, // ER_COLUMNACCESS_DENIED_ERROR:只授了部分列的 SELECT 权限
		1146, // ER_NO_SUCH_TABLE:实例没有这两张表(MySQL 8.0 以前、MariaDB 等)
		1227: // ER_SPECIFIC_ACCESS_DENIED_ERROR:缺某项全局权限
		return true
	}
	return false
}

// failOrSkipOnLockWaitError 处理 awaitLockWaiters 的错误(与 go/friend 同名助手同一口径)。读不了 performance_schema 时:
// 验收模式(TRADE_REQUIRE_MYSQL_TESTS)下判红 —— 这几条是收藏 S→X 成环与否唯一的确定性证据,不许静默跳过;
// 否则跳过(不代表通过)。其余错误(等待者没到齐、ctx 结束、非权限类查询错误)一律判红:编排失败本身就说明
// 锁行为与推演不符,或者用例卡住了。
func failOrSkipOnLockWaitError(t *testing.T, err error, expectation string) {
	t.Helper()
	if errors.Is(err, errLockWaitsUnobservable) {
		if requireMySQLTests() {
			t.Fatalf("%s 已设置(验收模式),无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限): %v",
				tradeRequireMySQLEnv, err)
		}
		t.Skipf("无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限),跳过 —— 不代表通过: %v", err)
	}
	t.Fatalf("夹具编排失败:%v —— %s", err, expectation)
}

// TestLockWaitsQueryErrorClassification 钉住 lockWaitsQueryError 的定性。不连库:带 integration 标签、不设 DSN 也照跑。
// 只有权限 / 对象缺失类错误号算"不可观测";ctx 结束与其余错误都必须判红 —— 2026-09-21 复审前这里把 ctx 超时
// 也包成了"不可观测",卡住的编排会被报成 SKIP。
func TestLockWaitsQueryErrorClassification(t *testing.T) {
	live := context.Background()
	for _, errNo := range []uint16{1044, 1142, 1143, 1146, 1227} {
		err := lockWaitsQueryError(live, fmt.Errorf("中间包了一层: %w", &mysql.MySQLError{Number: errNo}))
		assert.ErrorIs(t, err, errLockWaitsUnobservable, "错误号 %d 是权限 / 对象缺失类,应归为不可观测", errNo)
	}
	for _, cause := range []error{
		&mysql.MySQLError{Number: 1213},
		mysql.ErrInvalidConn,
		errors.New("任意非 MySQL 错误"),
	} {
		err := lockWaitsQueryError(live, cause)
		assert.NotErrorIs(t, err, errLockWaitsUnobservable, "%v 不是权限 / 对象缺失类错误,必须判红", cause)
		assert.ErrorIs(t, err, cause, "判红时要带出原始错误")
	}

	// ctx 已结束时以 ctx 为准判红,哪怕查询错误碰巧是权限类:卡住不许被报成 SKIP。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := lockWaitsQueryError(canceled, &mysql.MySQLError{Number: 1142})
	assert.NotErrorIs(t, err, errLockWaitsUnobservable)
	assert.ErrorIs(t, err, context.Canceled)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	err = lockWaitsQueryError(expired, mysql.ErrInvalidConn)
	assert.NotErrorIs(t, err, errLockWaitsUnobservable)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
