package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tradepb "proto/trade"
	"shared/assetop"
)

// ErrListingNotFound 表示 GetListing 查无此行。与存储故障区分:前者是业务拒绝,后者是 fault。
var ErrListingNotFound = errors.New("trade: listing not found")

// ListingQuery 是浏览列表的查询描述(P1 规格 §6 BrowseListings 第 5 条)。
// 由 logic 在完成全部校验后填写;本包只负责把它翻译成参数化 SQL,不再做业务判断。
type ListingQuery struct {
	// MarketZone:0 = 不按分区过滤(global 且 zone_filter=0);否则 market_zone = ?。
	MarketZone uint32
	// Tab 决定状态与时间窗:PUBLIC_NOTICE = 公示中;ON_SALE = 寄售中(含 LOCKED)。其余值拒绝。
	Tab tradepb.ListingTab
	// Category 必填。
	Category tradepb.ListingCategory
	// Subcategory:0 = 全部。
	Subcategory uint32
	// TitleLikePattern:空 = 不搜。非空时必须**已经**用 logic.EscapeLike 以 '!' 为转义符转义,
	// 并由调用方加好 % 通配;SQL 固定写 ESCAPE '!',不依赖 sql_mode 的反斜杠语义。
	TitleLikePattern string
	// SearchListingID:非 0 时与标题搜索取并集(listing_id = ? OR title LIKE ?)。
	// 只在 TitleLikePattern 非空时生效(纯数字搜索词同时按编号精确匹配)。
	SearchListingID uint64
	// FavoritesOf:非 0 时只看该玩家收藏过的商品。
	FavoritesOf uint64
	// Sort 从固定映射取 ORDER BY 片段,见 listingOrderBy。
	Sort tradepb.ListingSort
	// NowMs:本次请求唯一一次取到的服务端时间(Unix 毫秒),公示 / 寄售窗口都按它判定。
	NowMs uint64
}

// ListingStore 是 logic 眼里的聚宝斋存储。*ListingRepo 是 MySQL 实现,单测注入 fake。
//
// 契约:
//   - 每个方法自带 StoreOpTimeout 上限(构造时传入),调用方 ctx 更早到期时以 ctx 为准;
//   - 返回的 error 一律是存储故障,调用方按 kServiceUnavailable 处理;唯一的例外是
//     GetListing 的 ErrListingNotFound;
//   - 分页参数由调用方钳制,本包不再二次钳制。
type ListingStore interface {
	CountListings(ctx context.Context, q ListingQuery) (uint64, error)
	QueryListings(ctx context.Context, q ListingQuery, offset, limit uint64) ([]*tradepb.TradeListingRecord, error)
	CountSellerListings(ctx context.Context, sellerPlayerID uint64) (uint64, error)
	// QuerySellerListings 按 listing_id DESC 返回卖家任意状态的商品(货架)。
	QuerySellerListings(ctx context.Context, sellerPlayerID uint64, offset, limit uint64) ([]*tradepb.TradeListingRecord, error)
	// GetListing 返回含 description 的完整行;不存在返回 ErrListingNotFound。
	GetListing(ctx context.Context, listingID uint64) (*tradepb.TradeListingRecord, error)
	InsertListing(ctx context.Context, rec *tradepb.TradeListingRecord) error
	// FavoriteIDs 返回 listingIDs 中该玩家已收藏的子集;listingIDs 为空时不查库。
	FavoriteIDs(ctx context.Context, playerID uint64, listingIDs []uint64) (map[uint64]bool, error)
	FavoriteExists(ctx context.Context, playerID, listingID uint64) (bool, error)
	CountFavorites(ctx context.Context, playerID uint64) (uint64, error)
	// InsertFavorite 幂等:重复收藏不报错,也不刷新原收藏时间(ODKU 的 no-op 更新,不是 INSERT IGNORE,见实现)。
	InsertFavorite(ctx context.Context, rec *tradepb.TradeFavoriteRecord) error
	// DeleteFavorite 幂等:行不存在也不报错。
	DeleteFavorite(ctx context.Context, playerID, listingID uint64) error
}

// 列清单。浏览 / 货架不取 description(最长 512 字符的 MEDIUMTEXT,列表用不上);详情取全列。
// 列名统一加反引号:level / status / description / version 在 MySQL / TiDB 里都是关键字(非保留),
// 统一写法免得以后加列时逐个判断。
const (
	listingSummaryColumns = "`listing_id`, `seller_player_id`, `seller_account`, `market_zone`, `seller_zone_at_listing`, " +
		"`category`, `subcategory`, `title`, `level`, `price_fen`, `status`, `summary`, `icon_key`, " +
		"`notice_end_ms`, `sale_end_ms`, `created_ms`, `updated_ms`, `version`"
	listingDetailColumns = listingSummaryColumns + ", `description`"
)

// ListingRepo 是 ListingStore 的 MySQL 实现:手写参数化 SQL,不拼任何用户输入。
type ListingRepo struct {
	db        *sql.DB
	opTimeout time.Duration
}

var _ ListingStore = (*ListingRepo)(nil)

// NewListingRepo。opTimeout 是每次调用的上限(constants.StoreOpTimeout),必须 > 0。
func NewListingRepo(db *sql.DB, opTimeout time.Duration) *ListingRepo {
	return &ListingRepo{db: db, opTimeout: opTimeout}
}

func (r *ListingRepo) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.opTimeout)
}

// CountListings 统计浏览条件命中的总数(分页的 total_count / page_count)。
func (r *ListingRepo) CountListings(ctx context.Context, q ListingQuery) (uint64, error) {
	where, args, err := buildListingFilter(q)
	if err != nil {
		return 0, err
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var total uint64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM trade_listing"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count trade_listing: %w", err)
	}
	return total, nil
}

// QueryListings 返回浏览条件命中的一页。
func (r *ListingRepo) QueryListings(ctx context.Context, q ListingQuery, offset, limit uint64) ([]*tradepb.TradeListingRecord, error) {
	where, args, err := buildListingFilter(q)
	if err != nil {
		return nil, err
	}
	orderBy, err := listingOrderBy(q.Sort, q.Tab)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + listingSummaryColumns + " FROM trade_listing" + where +
		" ORDER BY " + orderBy + " LIMIT ? OFFSET ?"
	// Clone:别让 append 与 buildListingFilter 返回的切片共用底层数组。
	queryArgs := append(slices.Clone(args), limit, offset)
	return r.queryListingRows(ctx, query, queryArgs)
}

// CountSellerListings 统计卖家任意状态的商品数(货架)。走 (seller_player_id, listing_id) 索引。
func (r *ListingRepo) CountSellerListings(ctx context.Context, sellerPlayerID uint64) (uint64, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var total uint64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trade_listing WHERE `seller_player_id` = ?", sellerPlayerID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count trade_listing by seller: %w", err)
	}
	return total, nil
}

// QuerySellerListings 返回卖家货架的一页,新上架在前。
func (r *ListingRepo) QuerySellerListings(ctx context.Context, sellerPlayerID uint64, offset, limit uint64) ([]*tradepb.TradeListingRecord, error) {
	query := "SELECT " + listingSummaryColumns + " FROM trade_listing WHERE `seller_player_id` = ?" +
		" ORDER BY `listing_id` DESC LIMIT ? OFFSET ?"
	return r.queryListingRows(ctx, query, []any{sellerPlayerID, limit, offset})
}

func (r *ListingRepo) queryListingRows(ctx context.Context, query string, args []any) ([]*tradepb.TradeListingRecord, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query trade_listing: %w", err)
	}
	defer rows.Close()

	var out []*tradepb.TradeListingRecord
	for rows.Next() {
		rec, err := scanListing(rows, false)
		if err != nil {
			return nil, fmt.Errorf("scan trade_listing: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trade_listing: %w", err)
	}
	return out, nil
}

// GetListing 按主键取完整行。
func (r *ListingRepo) GetListing(ctx context.Context, listingID uint64) (*tradepb.TradeListingRecord, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	row := r.db.QueryRowContext(ctx,
		"SELECT "+listingDetailColumns+" FROM trade_listing WHERE `listing_id` = ?", listingID)
	rec, err := scanListing(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrListingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get trade_listing %d: %w", listingID, err)
	}
	return rec, nil
}

// InsertListing 插入一条完整商品行。主键冲突按故障返回:listing_id 来自号段,撞号说明发号源出了问题,
// 绝不能静默覆盖(所以不用 INSERT IGNORE / ON DUPLICATE KEY UPDATE)。
func (r *ListingRepo) InsertListing(ctx context.Context, rec *tradepb.TradeListingRecord) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO trade_listing ("+listingDetailColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rec.GetListingId(), rec.GetSellerPlayerId(), rec.GetSellerAccount(), rec.GetMarketZone(), rec.GetSellerZoneAtListing(),
		int32(rec.GetCategory()), rec.GetSubcategory(), rec.GetTitle(), rec.GetLevel(), rec.GetPriceFen(),
		int32(rec.GetStatus()), rec.GetSummary(), rec.GetIconKey(),
		rec.GetNoticeEndMs(), rec.GetSaleEndMs(), rec.GetCreatedMs(), rec.GetUpdatedMs(), rec.GetVersion(),
		rec.GetDescription(),
	)
	if err != nil {
		return fmt.Errorf("insert trade_listing %d: %w", rec.GetListingId(), err)
	}
	return nil
}

// FavoriteIDs 批量查本页哪些商品已被该玩家收藏。一页最多 MaxPageSize(20)个 id,IN 列表很短。
func (r *ListingRepo) FavoriteIDs(ctx context.Context, playerID uint64, listingIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(listingIDs))
	if len(listingIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(listingIDs)), ",")
	args := make([]any, 0, len(listingIDs)+1)
	args = append(args, playerID)
	for _, id := range listingIDs {
		args = append(args, id)
	}

	ctx, cancel := r.bounded(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		"SELECT `listing_id` FROM trade_favorite WHERE `player_id` = ? AND `listing_id` IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("query trade_favorite: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan trade_favorite: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trade_favorite: %w", err)
	}
	return out, nil
}

// FavoriteExists 按主键 (player_id, listing_id) 判断是否已收藏。
func (r *ListingRepo) FavoriteExists(ctx context.Context, playerID, listingID uint64) (bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var one int
	err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM trade_favorite WHERE `player_id` = ? AND `listing_id` = ?", playerID, listingID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get trade_favorite: %w", err)
	}
	return true, nil
}

// CountFavorites 统计玩家收藏条数(主键前缀 player_id)。
func (r *ListingRepo) CountFavorites(ctx context.Context, playerID uint64) (uint64, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var total uint64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trade_favorite WHERE `player_id` = ?", playerID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count trade_favorite: %w", err)
	}
	return total, nil
}

// insertFavoriteSQL 是收藏写入。ODKU 的 no-op 更新(created_ms = created_ms)让重复收藏(客户端重试 / 并发双击 /
// 多端)幂等,且不刷新原收藏时间。
//
// 为什么不是 INSERT IGNORE(2026-09-21 死锁审计 #9):收藏 → 取消 → 再收藏时,(player, listing) 主键可能是刚被
// DeleteFavorite 删掉、尚未 purge 的 delete-marked 记录。成环有一个前提:**必须有第三方正持着这条删除标记记录的 X**,
// 两个 INSERT IGNORE 同时排在它后面。本调用路径上的第三方只有两种:
//   - 未提交的取消收藏 DELETE(自动提交语句从取锁到提交之间同样持着 X,只是窗口窄);
//   - 先到的收藏已把删除标记记录复活、随后回滚(ctx 超时、断连、被选为死锁牺牲者)。
//
// 第三方一放锁,两个排队者的重复键检查**同时**拿到 **S**(彼此相容),都判定"只是删除标记、不算重复",
// 要复活它就各自申请 **X**,被对方的 S 挡住 → 1213。
// 没有第三方时两方交错成不了环:单条 INSERT 自己的 S 与随后的 X 是在同一个 mini-transaction 的页闩下连续取得的,
// 另一方插不进来拿 S,只能排在它的 X 后面,等它提交后看见活行(或回滚后独自复活)。
// ODKU 的重复键检查直接取 X,排队者只能一个一个拿到:先到者复活记录,后到者看见活行走 no-op 更新,不存在升级。
// (取锁细节按手册 "Locks Set by Different SQL Statements" 与 InnoDB 插入路径的已知行为推演,以真库回归为准:
// listing_repo_integration_test.go 的 TestLegacyInsertIgnoreFavoriteDeadlocksOnDeleteMarkedRow 是红对照,
// TestInsertFavoriteSQLRevivesDeleteMarkedRowWithoutDeadlock 钉住本语句。)
const insertFavoriteSQL = "INSERT INTO trade_favorite (`player_id`, `listing_id`, `created_ms`) VALUES (?, ?, ?)" +
	" ON DUPLICATE KEY UPDATE `created_ms` = `created_ms`"

// InsertFavorite 收藏,幂等。
//
// 这一条语句包在 assetop.WithTxRetry 的单语句 RC 事务里,只为复用它的有界重试与可取消退避。
// ODKU 已经消掉了删除标记记录上的 S→X 环(见 insertFavoriteSQL,也就是手册三会话例说的那种);剩下的 1213
// 只可能来自两类 InnoDB 固有情形,SQL 层去不掉,与本服务的锁序无关:
//   - purge 在排队期间清掉了删除标记记录;
//   - 同键全新插入,先到者回滚(它插入的记录随回滚被物理移除)。
//
// 两者都是"排队者等着的那条记录物理消失":排队中的 X 被继承成下一条记录上的间隙锁(ODKU 的重复键检查带
// duplicates 标记,RC 下这把锁照样被继承),随后各自的插入意向锁被对方的间隙锁挡住而互等,InnoDB 当场牺牲其一。
// 收敛:被牺牲方整条回滚后重跑,记录要么已被胜者插入(走 no-op 更新),要么仍不存在(普通插入);只有再次撞上
// "同键插入 + 回滚 / purge"才会再成环,而每成一次环都有一方完成。重试有上限(txRetryAttempts)、带抖动退避、
// 受 opTimeout 与 ctx 约束;极端同键风暴下重试用尽则返回错误,不会挂住。语句幂等,重跑安全;
// 不重试的话玩家看到的是一次"服务不可用"。代价是多两次 BEGIN / COMMIT 往返,收藏是低频操作。
func (r *ListingRepo) InsertFavorite(ctx context.Context, rec *tradepb.TradeFavoriteRecord) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	if err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertFavoriteSQL, rec.GetPlayerId(), rec.GetListingId(), rec.GetCreatedMs())
		return err
	}); err != nil {
		return fmt.Errorf("insert trade_favorite: %w", err)
	}
	return nil
}

// DeleteFavorite 取消收藏,幂等。
func (r *ListingRepo) DeleteFavorite(ctx context.Context, playerID, listingID uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		"DELETE FROM trade_favorite WHERE `player_id` = ? AND `listing_id` = ?", playerID, listingID); err != nil {
		return fmt.Errorf("delete trade_favorite: %w", err)
	}
	return nil
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的公共 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanListing 按 listingSummaryColumns(+ description)的列序把一行扫进 TradeListingRecord。
// 枚举列先扫进 int32 再转型,不依赖 database/sql 对具名整数类型的反射转换。
func scanListing(row rowScanner, withDescription bool) (*tradepb.TradeListingRecord, error) {
	rec := &tradepb.TradeListingRecord{}
	var category, status int32
	dest := []any{
		&rec.ListingId, &rec.SellerPlayerId, &rec.SellerAccount, &rec.MarketZone, &rec.SellerZoneAtListing,
		&category, &rec.Subcategory, &rec.Title, &rec.Level, &rec.PriceFen, &status, &rec.Summary, &rec.IconKey,
		&rec.NoticeEndMs, &rec.SaleEndMs, &rec.CreatedMs, &rec.UpdatedMs, &rec.Version,
	}
	if withDescription {
		dest = append(dest, &rec.Description)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	rec.Category = tradepb.ListingCategory(category)
	rec.Status = tradepb.ListingStatus(status)
	return rec, nil
}

// buildListingFilter 把 ListingQuery 翻译成 " WHERE ..." 与参数。全部条件参数化;
// 公示 / 寄售窗口按 P1 规格 §6 BrowseListings 第 5 条:
//
//	公示中:status = LISTED AND notice_end_ms > now
//	寄售中:status IN (LISTED, LOCKED) AND notice_end_ms <= now AND sale_end_ms > now
func buildListingFilter(q ListingQuery) (string, []any, error) {
	var conds []string
	var args []any

	switch q.Tab {
	case tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE:
		conds = append(conds, "`status` = ?", "`notice_end_ms` > ?")
		args = append(args, int32(tradepb.ListingStatus_LISTING_STATUS_LISTED), q.NowMs)
	case tradepb.ListingTab_LISTING_TAB_ON_SALE:
		conds = append(conds, "`status` IN (?, ?)", "`notice_end_ms` <= ?", "`sale_end_ms` > ?")
		args = append(args,
			int32(tradepb.ListingStatus_LISTING_STATUS_LISTED), int32(tradepb.ListingStatus_LISTING_STATUS_LOCKED),
			q.NowMs, q.NowMs)
	default:
		return "", nil, fmt.Errorf("trade: unsupported listing tab %d", q.Tab)
	}
	if q.Category == tradepb.ListingCategory_LISTING_CATEGORY_UNSPECIFIED {
		return "", nil, errors.New("trade: listing query requires a category")
	}

	if q.MarketZone != 0 {
		conds = append(conds, "`market_zone` = ?")
		args = append(args, q.MarketZone)
	}
	conds = append(conds, "`category` = ?")
	args = append(args, int32(q.Category))
	if q.Subcategory != 0 {
		conds = append(conds, "`subcategory` = ?")
		args = append(args, q.Subcategory)
	}
	if q.TitleLikePattern != "" {
		if q.SearchListingID != 0 {
			conds = append(conds, "(`listing_id` = ? OR `title` LIKE ? ESCAPE '!')")
			args = append(args, q.SearchListingID, q.TitleLikePattern)
		} else {
			conds = append(conds, "`title` LIKE ? ESCAPE '!'")
			args = append(args, q.TitleLikePattern)
		}
	}
	if q.FavoritesOf != 0 {
		conds = append(conds,
			"EXISTS (SELECT 1 FROM trade_favorite f WHERE f.`player_id` = ? AND f.`listing_id` = trade_listing.`listing_id`)")
		args = append(args, q.FavoritesOf)
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// listingOrderBy 是 ORDER BY 片段的固定映射:只能从这里取,绝不拼用户输入。
// 每种排序都以 listing_id 作最终决胜列,保证翻页不重不漏。
func listingOrderBy(sort tradepb.ListingSort, tab tradepb.ListingTab) (string, error) {
	switch sort {
	case tradepb.ListingSort_LISTING_SORT_DEFAULT:
		return "`listing_id` DESC", nil
	case tradepb.ListingSort_LISTING_SORT_PRICE_ASC:
		return "`price_fen` ASC, `listing_id` ASC", nil
	case tradepb.ListingSort_LISTING_SORT_PRICE_DESC:
		return "`price_fen` DESC, `listing_id` DESC", nil
	case tradepb.ListingSort_LISTING_SORT_LEVEL_DESC:
		return "`level` DESC, `listing_id` DESC", nil
	case tradepb.ListingSort_LISTING_SORT_REMAINING_ASC:
		// 公示列表按公示结束、寄售列表按寄售结束。
		if tab == tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE {
			return "`notice_end_ms` ASC, `listing_id` ASC", nil
		}
		return "`sale_end_ms` ASC, `listing_id` ASC", nil
	default:
		return "", fmt.Errorf("trade: unsupported listing sort %d", sort)
	}
}
