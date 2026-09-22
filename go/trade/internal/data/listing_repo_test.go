package data

import (
	"reflect"
	"strings"
	"testing"

	tradepb "proto/trade"
)

// SQL 构造的纯单测:不连库,只钉住条件、参数顺序与排序映射。真库行为见 listing_repo_integration_test.go。

const nowMs = uint64(1_800_000_000_000)

var (
	listed = int32(tradepb.ListingStatus_LISTING_STATUS_LISTED)
	locked = int32(tradepb.ListingStatus_LISTING_STATUS_LOCKED)
	weapon = tradepb.ListingCategory_LISTING_CATEGORY_WEAPON
)

func TestBuildListingFilter(t *testing.T) {
	cases := []struct {
		name     string
		q        ListingQuery
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "公示中 + 类目",
			q:        ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE, Category: weapon, NowMs: nowMs},
			wantSQL:  " WHERE `status` = ? AND `notice_end_ms` > ? AND `category` = ?",
			wantArgs: []any{listed, nowMs, int32(weapon)},
		},
		{
			name:     "寄售中含 LOCKED + 分区 + 子类",
			q:        ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_ON_SALE, Category: weapon, Subcategory: 2, MarketZone: 7, NowMs: nowMs},
			wantSQL:  " WHERE `status` IN (?, ?) AND `notice_end_ms` <= ? AND `sale_end_ms` > ? AND `market_zone` = ? AND `category` = ? AND `subcategory` = ?",
			wantArgs: []any{listed, locked, nowMs, nowMs, uint32(7), int32(weapon), uint32(2)},
		},
		{
			name:     "非数字搜索只按标题",
			q:        ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_ON_SALE, Category: weapon, TitleLikePattern: "%a!_b%", NowMs: nowMs},
			wantSQL:  " WHERE `status` IN (?, ?) AND `notice_end_ms` <= ? AND `sale_end_ms` > ? AND `category` = ? AND `title` LIKE ? ESCAPE '!'",
			wantArgs: []any{listed, locked, nowMs, nowMs, int32(weapon), "%a!_b%"},
		},
		{
			name:     "数字搜索同时按编号",
			q:        ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_ON_SALE, Category: weapon, TitleLikePattern: "%123%", SearchListingID: 123, NowMs: nowMs},
			wantSQL:  " WHERE `status` IN (?, ?) AND `notice_end_ms` <= ? AND `sale_end_ms` > ? AND `category` = ? AND (`listing_id` = ? OR `title` LIKE ? ESCAPE '!')",
			wantArgs: []any{listed, locked, nowMs, nowMs, int32(weapon), uint64(123), "%123%"},
		},
		{
			name: "只看收藏",
			q:    ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE, Category: weapon, FavoritesOf: 42, NowMs: nowMs},
			wantSQL: " WHERE `status` = ? AND `notice_end_ms` > ? AND `category` = ? AND " +
				"EXISTS (SELECT 1 FROM trade_favorite f WHERE f.`player_id` = ? AND f.`listing_id` = trade_listing.`listing_id`)",
			wantArgs: []any{listed, nowMs, int32(weapon), uint64(42)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSQL, gotArgs, err := buildListingFilter(tc.q)
			if err != nil {
				t.Fatalf("buildListingFilter: %v", err)
			}
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL\n got: %s\nwant: %s", gotSQL, tc.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Errorf("args\n got: %#v\nwant: %#v", gotArgs, tc.wantArgs)
			}
			if strings.Count(gotSQL, "?") != len(gotArgs) {
				t.Errorf("占位符 %d 个,参数 %d 个", strings.Count(gotSQL, "?"), len(gotArgs))
			}
		})
	}
}

func TestBuildListingFilterRejectsIncompleteQuery(t *testing.T) {
	if _, _, err := buildListingFilter(ListingQuery{Category: weapon}); err == nil {
		t.Error("缺 tab 必须报错,不能退化成不带状态条件的全表查询")
	}
	if _, _, err := buildListingFilter(ListingQuery{Tab: tradepb.ListingTab_LISTING_TAB_ON_SALE}); err == nil {
		t.Error("缺类目必须报错")
	}
}

func TestListingOrderBy(t *testing.T) {
	notice := tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE
	onSale := tradepb.ListingTab_LISTING_TAB_ON_SALE
	cases := []struct {
		sort tradepb.ListingSort
		tab  tradepb.ListingTab
		want string
	}{
		{tradepb.ListingSort_LISTING_SORT_DEFAULT, onSale, "`listing_id` DESC"},
		{tradepb.ListingSort_LISTING_SORT_PRICE_ASC, onSale, "`price_fen` ASC, `listing_id` ASC"},
		{tradepb.ListingSort_LISTING_SORT_PRICE_DESC, onSale, "`price_fen` DESC, `listing_id` DESC"},
		{tradepb.ListingSort_LISTING_SORT_LEVEL_DESC, onSale, "`level` DESC, `listing_id` DESC"},
		{tradepb.ListingSort_LISTING_SORT_REMAINING_ASC, notice, "`notice_end_ms` ASC, `listing_id` ASC"},
		{tradepb.ListingSort_LISTING_SORT_REMAINING_ASC, onSale, "`sale_end_ms` ASC, `listing_id` ASC"},
	}
	for _, tc := range cases {
		got, err := listingOrderBy(tc.sort, tc.tab)
		if err != nil || got != tc.want {
			t.Errorf("listingOrderBy(%v, %v) = (%q, %v), want %q", tc.sort, tc.tab, got, err, tc.want)
		}
	}
	if _, err := listingOrderBy(tradepb.ListingSort(99), onSale); err == nil {
		t.Error("未知排序必须报错,不能回落到任意 ORDER BY")
	}
	// proto 里每个排序值都必须有映射,新增排序漏登记会在这里红。
	for value := range tradepb.ListingSort_name {
		if _, err := listingOrderBy(tradepb.ListingSort(value), onSale); err != nil {
			t.Errorf("排序 %d 没有 ORDER BY 映射: %v", value, err)
		}
	}
}

// TestInsertFavoriteSQLIsUpsertNotInsertIgnore 钉住收藏写入的锁模式(2026-09-21 死锁审计 #9)。
//
// 重复键检查取什么锁由语句形状决定:INSERT IGNORE 取 S,排在同一条删除标记记录后面的并发收藏会同时拿到,
// 再各自升 X 就成环(1213);ODKU 直接取 X,并发者只会排队。改回 INSERT IGNORE 之后重复收藏照样幂等、
// 功能用例照样绿,死锁却回来了 —— 而能抓住它的真库回归(listing_repo_integration_test.go 末尾)默认不跑,
// 所以在这里用纯文本再拦一道。
func TestInsertFavoriteSQLIsUpsertNotInsertIgnore(t *testing.T) {
	// 大写 + 折叠空白,换行、多空格、大小写的改写都不影响判定。
	normalized := strings.ToUpper(strings.Join(strings.Fields(insertFavoriteSQL), " "))
	if !strings.Contains(normalized, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("insertFavoriteSQL 必须是 INSERT … ON DUPLICATE KEY UPDATE(重复键检查直接取 X),got: %s", insertFavoriteSQL)
	}
	// 按词比对而不是只找 "INSERT IGNORE" 子串:INSERT LOW_PRIORITY IGNORE 之类的写法同样取 S。
	for _, word := range strings.Fields(normalized) {
		if word == "IGNORE" {
			t.Errorf("insertFavoriteSQL 不得带 IGNORE(重复键检查取 S,删除标记记录上会 S→X 成环),got: %s", insertFavoriteSQL)
		}
	}
}

func TestListingColumnsMatchScanOrder(t *testing.T) {
	// scanListing 的目标数与列清单必须一致,否则 Scan 在运行期报列数不符。
	summaryCols := strings.Count(listingSummaryColumns, ",") + 1
	detailCols := strings.Count(listingDetailColumns, ",") + 1
	if summaryCols != 18 || detailCols != 19 {
		t.Fatalf("列数 summary=%d detail=%d,应为 18 / 19(与 scanListing 的目标一致)", summaryCols, detailCols)
	}
	// TradeListingRecord 共 19 个字段,详情列清单必须覆盖全部字段。
	if fields := (&tradepb.TradeListingRecord{}).ProtoReflect().Descriptor().Fields().Len(); fields != detailCols {
		t.Fatalf("TradeListingRecord 有 %d 个字段,详情列清单只有 %d 列:proto 加字段后要同步列清单与 scanListing", fields, detailCols)
	}
	fields := (&tradepb.TradeListingRecord{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		name := "`" + string(fields.Get(i).Name()) + "`"
		if !strings.Contains(listingDetailColumns, name) {
			t.Errorf("详情列清单缺少列 %s", name)
		}
	}
}
