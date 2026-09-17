package logic

import (
	"strings"
	"testing"
	"time"

	"trade/internal/constants"

	tradepb "proto/trade"
)

func listingRec(status tradepb.ListingStatus, noticeEnd, saleEnd uint64, zone uint32) *tradepb.TradeListingRecord {
	return &tradepb.TradeListingRecord{Status: status, NoticeEndMs: noticeEnd, SaleEndMs: saleEnd, MarketZone: zone}
}

func TestPhase(t *testing.T) {
	const now = uint64(1000)
	listed := tradepb.ListingStatus_LISTING_STATUS_LISTED
	cases := []struct {
		name string
		rec  *tradepb.TradeListingRecord
		want tradepb.ListingPhase
	}{
		{"公示中", listingRec(listed, 2000, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_PUBLIC_NOTICE},
		{"now == notice_end 进入寄售", listingRec(listed, 1000, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_ON_SALE},
		{"无公示期直接寄售", listingRec(listed, 0, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_ON_SALE},
		{"now == sale_end 结束", listingRec(listed, 500, 1000, 1), tradepb.ListingPhase_LISTING_PHASE_ENDED},
		{"LOCKED", listingRec(tradepb.ListingStatus_LISTING_STATUS_LOCKED, 500, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_LOCKED},
		{"LOCKED 不看时间", listingRec(tradepb.ListingStatus_LISTING_STATUS_LOCKED, 0, 500, 1), tradepb.ListingPhase_LISTING_PHASE_LOCKED},
		{"SOLD", listingRec(tradepb.ListingStatus_LISTING_STATUS_SOLD, 0, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_ENDED},
		{"ESCROWING", listingRec(tradepb.ListingStatus_LISTING_STATUS_ESCROWING, 2000, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_ENDED},
		{"UNSPECIFIED", listingRec(tradepb.ListingStatus_LISTING_STATUS_UNSPECIFIED, 2000, 3000, 1), tradepb.ListingPhase_LISTING_PHASE_ENDED},
	}
	for _, tc := range cases {
		if got := Phase(tc.rec, now); got != tc.want {
			t.Errorf("%s: Phase = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestVisibleToBuyer(t *testing.T) {
	const now = uint64(1000)
	listed := tradepb.ListingStatus_LISTING_STATUS_LISTED
	zoneScope := tradepb.MarketScope_MARKET_SCOPE_ZONE
	globalScope := tradepb.MarketScope_MARKET_SCOPE_GLOBAL
	cases := []struct {
		name   string
		rec    *tradepb.TradeListingRecord
		scope  tradepb.MarketScope
		caller uint32
		want   bool
	}{
		{"zone 同区寄售中", listingRec(listed, 0, 3000, 1), zoneScope, 1, true},
		{"zone 同区公示中", listingRec(listed, 2000, 3000, 1), zoneScope, 1, true},
		{"zone 别区", listingRec(listed, 0, 3000, 1), zoneScope, 2, false},
		{"zone 调用者 home_zone=0 一律不可见", listingRec(listed, 0, 3000, 0), zoneScope, 0, false},
		{"global 不看分区", listingRec(listed, 0, 3000, 1), globalScope, 0, true},
		{"LOCKED 寄售期内可见", listingRec(tradepb.ListingStatus_LISTING_STATUS_LOCKED, 0, 3000, 1), zoneScope, 1, true},
		{"LISTED 已过寄售期", listingRec(listed, 0, 1000, 1), globalScope, 0, false},
		{"LOCKED 已过寄售期", listingRec(tradepb.ListingStatus_LISTING_STATUS_LOCKED, 0, 999, 1), globalScope, 0, false},
		{"SOLD", listingRec(tradepb.ListingStatus_LISTING_STATUS_SOLD, 0, 3000, 1), globalScope, 0, false},
		{"scope 未指定 fail-closed", listingRec(listed, 0, 3000, 1), tradepb.MarketScope_MARKET_SCOPE_UNSPECIFIED, 1, false},
	}
	for _, tc := range cases {
		if got := VisibleToBuyer(tc.rec, now, tc.scope, tc.caller); got != tc.want {
			t.Errorf("%s: VisibleToBuyer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClampPageSize(t *testing.T) {
	cases := []struct{ requested, def, max, want uint32 }{
		{0, 20, 20, 20},
		{4, 20, 20, 4},
		{20, 20, 20, 20},
		{50, 20, 20, 20},
		{0, 10, 20, 10},
	}
	for _, tc := range cases {
		if got := ClampPageSize(tc.requested, tc.def, tc.max); got != tc.want {
			t.Errorf("ClampPageSize(%d, %d, %d) = %d, want %d", tc.requested, tc.def, tc.max, got, tc.want)
		}
	}
}

func TestPageWindow(t *testing.T) {
	cases := []struct {
		name                string
		total               uint64
		page, size, maxPage uint32
		wantPage, wantCount uint32
		wantOffset          uint64
	}{
		{"空结果至少一页", 0, 1, 20, 100, 1, 1, 0},
		{"page=0 视为 1", 45, 0, 20, 100, 1, 3, 0},
		{"正好整除", 40, 2, 20, 100, 2, 2, 20},
		{"超过末页按末页", 45, 9999, 20, 100, 3, 3, 40},
		{"页码上限", 100000, 500, 20, 100, 100, 5000, 1980},
		{"maxPage=0 不设上限", 100000, 500, 20, 0, 500, 5000, 9980},
		{"页长 4", 9, 3, 4, 100, 3, 3, 8},
	}
	for _, tc := range cases {
		page, count, offset := PageWindow(tc.total, tc.page, tc.size, tc.maxPage)
		if page != tc.wantPage || count != tc.wantCount || offset != tc.wantOffset {
			t.Errorf("%s: PageWindow = (page=%d count=%d offset=%d), want (%d, %d, %d)",
				tc.name, page, count, offset, tc.wantPage, tc.wantCount, tc.wantOffset)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"青锋剑":    "青锋剑",
		"100%":   "100!%",
		"a_b":    "a!_b",
		"a!b":    "a!!b",
		"!%_":    "!!!%!_",
		"%%":     "!%!%",
		`a\b`:    `a\b`, // 反斜杠不是转义符,原样保留
		"SMK-1-": "SMK-1-",
	}
	for in, want := range cases {
		if got := EscapeLike(in); got != want {
			t.Errorf("EscapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidText(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want bool
	}{
		{"空串", "", 64, true},
		{"中文", "青锋剑", 64, true},
		{"恰好上限", strings.Repeat("剑", 64), 64, true},
		{"超过上限按字符数", strings.Repeat("剑", 65), 64, false},
		{"换行", "a\nb", 64, false},
		{"制表", "a\tb", 64, false},
		{"NUL", "a\x00", 64, false},
		{"DEL", "a\x7f", 64, false},
		{"C1 控制字符", "a\u0085", 64, false},
		{"非法 UTF-8", "a\xff", 64, false},
	}
	for _, tc := range cases {
		if got := ValidText(tc.s, tc.max); got != tc.want {
			t.Errorf("%s: ValidText = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidIconKey(t *testing.T) {
	cases := map[string]bool{
		"":                      true,
		"icon_sword_01":         true,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
		"Icon":                  false,
		"icon-sword":            false,
		"icon sword":            false,
		"图标":                    false,
		"../etc":                false,
	}
	for key, want := range cases {
		if got := ValidIconKey(key); got != want {
			t.Errorf("ValidIconKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestNormalizeSearch(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"  青锋  ", "青锋", true},
		{" " + strings.Repeat("剑", 64) + " ", strings.Repeat("剑", 64), true},
		{strings.Repeat("剑", 65), "", false},
		{"a\x00b", "", false},
		{"a\xffb", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeSearch(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("NormalizeSearch(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestEnumValidators(t *testing.T) {
	if !ValidTab(tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE) || !ValidTab(tradepb.ListingTab_LISTING_TAB_ON_SALE) ||
		ValidTab(tradepb.ListingTab_LISTING_TAB_UNSPECIFIED) || ValidTab(tradepb.ListingTab(3)) {
		t.Error("ValidTab 只接受 1、2")
	}
	if !ValidSection(tradepb.ListingSection_LISTING_SECTION_CONSIGNMENT) || !ValidSection(tradepb.ListingSection_LISTING_SECTION_AUCTION) ||
		ValidSection(tradepb.ListingSection_LISTING_SECTION_UNSPECIFIED) || ValidSection(tradepb.ListingSection(3)) {
		t.Error("ValidSection 只接受 1、2")
	}
	for value := range tradepb.ListingSort_name {
		if !ValidSort(tradepb.ListingSort(value)) {
			t.Errorf("proto 声明的排序 %d 必须合法", value)
		}
	}
	if ValidSort(tradepb.ListingSort(5)) || ValidSort(tradepb.ListingSort(-1)) {
		t.Error("未声明的排序必须非法")
	}
	weapon := tradepb.ListingCategory_LISTING_CATEGORY_WEAPON
	if !ValidCategory(weapon, 0) || !ValidCategory(weapon, 5) || ValidCategory(weapon, 6) {
		t.Error("武器子类上限 5")
	}
	if !ValidCategory(tradepb.ListingCategory_LISTING_CATEGORY_CURRENCY, 0) ||
		ValidCategory(tradepb.ListingCategory_LISTING_CATEGORY_CURRENCY, 1) {
		t.Error("游戏币没有子类")
	}
	if ValidCategory(tradepb.ListingCategory_LISTING_CATEGORY_UNSPECIFIED, 0) || ValidCategory(tradepb.ListingCategory(10), 0) {
		t.Error("类目必须在 1..9")
	}
}

// validSeedRequest 是一份能通过 ValidSeedRequest 的种子请求,负向用例在它上面逐项改坏。
func validSeedRequest() *tradepb.SeedListingRequest {
	return &tradepb.SeedListingRequest{
		SellerPlayerId:   playerA,
		Category:         tradepb.ListingCategory_LISTING_CATEGORY_WEAPON,
		Subcategory:      1,
		Title:            "SMK-1-A",
		Level:            10,
		PriceFen:         500,
		Summary:          "摘要",
		Description:      "描述",
		IconKey:          "icon_sword",
		NoticeDurationMs: 0,
		SaleDurationMs:   uint64(time.Hour.Milliseconds()),
	}
}

func TestValidSeedRequest(t *testing.T) {
	if !ValidSeedRequest(validSeedRequest()) {
		t.Fatal("基准种子请求应合法")
	}
	maxSale := uint64(constants.MaxSaleDuration.Milliseconds())
	maxNotice := uint64(constants.MaxNoticeDuration.Milliseconds())

	valid := []struct {
		name   string
		mutate func(in *tradepb.SeedListingRequest)
	}{
		{"寄售期恰好上限", func(in *tradepb.SeedListingRequest) { in.SaleDurationMs = maxSale }},
		{"公示期恰好上限", func(in *tradepb.SeedListingRequest) { in.NoticeDurationMs = maxNotice }},
		{"价格恰好上限", func(in *tradepb.SeedListingRequest) { in.PriceFen = constants.MaxPriceFen }},
		{"等级恰好上限", func(in *tradepb.SeedListingRequest) { in.Level = constants.MaxLevel }},
		{"空图标键", func(in *tradepb.SeedListingRequest) { in.IconKey = "" }},
		{"空摘要与描述", func(in *tradepb.SeedListingRequest) { in.Summary, in.Description = "", "" }},
		{"游戏币无子类", func(in *tradepb.SeedListingRequest) {
			in.Category, in.Subcategory = tradepb.ListingCategory_LISTING_CATEGORY_CURRENCY, 0
		}},
	}
	for _, tc := range valid {
		in := validSeedRequest()
		tc.mutate(in)
		if !ValidSeedRequest(in) {
			t.Errorf("%s: 应合法", tc.name)
		}
	}

	invalid := []struct {
		name   string
		mutate func(in *tradepb.SeedListingRequest)
	}{
		{"seller=0", func(in *tradepb.SeedListingRequest) { in.SellerPlayerId = 0 }},
		{"类目 0", func(in *tradepb.SeedListingRequest) {
			in.Category = tradepb.ListingCategory_LISTING_CATEGORY_UNSPECIFIED
		}},
		{"类目 10", func(in *tradepb.SeedListingRequest) { in.Category = tradepb.ListingCategory(10) }},
		{"子类越界", func(in *tradepb.SeedListingRequest) { in.Subcategory = 6 }},
		{"标题为空", func(in *tradepb.SeedListingRequest) { in.Title = "" }},
		{"标题全空白", func(in *tradepb.SeedListingRequest) { in.Title = "   " }},
		{"标题超长", func(in *tradepb.SeedListingRequest) { in.Title = strings.Repeat("剑", constants.MaxTitleRunes+1) }},
		{"标题含换行", func(in *tradepb.SeedListingRequest) { in.Title = "a\nb" }},
		{"摘要超长", func(in *tradepb.SeedListingRequest) { in.Summary = strings.Repeat("a", constants.MaxSummaryRunes+1) }},
		{"描述超长", func(in *tradepb.SeedListingRequest) {
			in.Description = strings.Repeat("a", constants.MaxDescriptionRunes+1)
		}},
		{"描述含控制字符", func(in *tradepb.SeedListingRequest) { in.Description = "a\x01" }},
		{"图标键字符集", func(in *tradepb.SeedListingRequest) { in.IconKey = "Icon-A" }},
		{"等级超限", func(in *tradepb.SeedListingRequest) { in.Level = constants.MaxLevel + 1 }},
		{"价格 0", func(in *tradepb.SeedListingRequest) { in.PriceFen = 0 }},
		{"价格超限", func(in *tradepb.SeedListingRequest) { in.PriceFen = constants.MaxPriceFen + 1 }},
		{"寄售期 0", func(in *tradepb.SeedListingRequest) { in.SaleDurationMs = 0 }},
		{"寄售期超限", func(in *tradepb.SeedListingRequest) { in.SaleDurationMs = maxSale + 1 }},
		{"公示期超限", func(in *tradepb.SeedListingRequest) { in.NoticeDurationMs = maxNotice + 1 }},
	}
	for _, tc := range invalid {
		in := validSeedRequest()
		tc.mutate(in)
		if ValidSeedRequest(in) {
			t.Errorf("%s: 应非法", tc.name)
		}
	}
}
