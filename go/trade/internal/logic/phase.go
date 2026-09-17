package logic

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"trade/internal/constants"

	tradepb "proto/trade"
)

// 本文件是逻辑层的纯函数:阶段推导、可见性、分页窗口、LIKE 转义与输入校验。
// 全部不碰 I/O、不读时钟(时间由调用方取一次后传入),单测直接打。

// Phase 按存储状态与同一次 now 推导展示阶段(P1-6):公示期 / 寄售期不落状态。
//
//	LISTED 且 now < notice_end_ms → PUBLIC_NOTICE
//	LISTED 且 now < sale_end_ms   → ON_SALE
//	LOCKED                        → LOCKED
//	其余(含 LISTED 已过寄售期)   → ENDED
func Phase(rec *tradepb.TradeListingRecord, nowMs uint64) tradepb.ListingPhase {
	switch rec.GetStatus() {
	case tradepb.ListingStatus_LISTING_STATUS_LISTED:
		if nowMs < rec.GetNoticeEndMs() {
			return tradepb.ListingPhase_LISTING_PHASE_PUBLIC_NOTICE
		}
		if nowMs < rec.GetSaleEndMs() {
			return tradepb.ListingPhase_LISTING_PHASE_ON_SALE
		}
		return tradepb.ListingPhase_LISTING_PHASE_ENDED
	case tradepb.ListingStatus_LISTING_STATUS_LOCKED:
		return tradepb.ListingPhase_LISTING_PHASE_LOCKED
	default:
		return tradepb.ListingPhase_LISTING_PHASE_ENDED
	}
}

// VisibleToBuyer 判断非卖家的调用者能否看到这件商品:
// status ∈ {LISTED, LOCKED} 且 now < sale_end_ms 且(scope=GLOBAL 或 market_zone == 调用者 home_zone)。
// scope=ZONE 时 callerHomeZone 必须是查到的非 0 值;传 0 一律不可见(fail-closed)。
func VisibleToBuyer(rec *tradepb.TradeListingRecord, nowMs uint64, scope tradepb.MarketScope, callerHomeZone uint32) bool {
	switch rec.GetStatus() {
	case tradepb.ListingStatus_LISTING_STATUS_LISTED, tradepb.ListingStatus_LISTING_STATUS_LOCKED:
	default:
		return false
	}
	if nowMs >= rec.GetSaleEndMs() {
		return false
	}
	switch scope {
	case tradepb.MarketScope_MARKET_SCOPE_GLOBAL:
		return true
	case tradepb.MarketScope_MARKET_SCOPE_ZONE:
		return callerHomeZone != 0 && rec.GetMarketZone() == callerHomeZone
	default:
		return false
	}
}

// ClampPageSize:0 → 默认页长;超过上限 → 上限。
func ClampPageSize(requested, defaultSize, maxSize uint32) uint32 {
	if requested == 0 {
		requested = defaultSize
	}
	if requested > maxSize {
		requested = maxSize
	}
	if requested == 0 { // 配置已保证 > 0,这里只防除零
		requested = 1
	}
	return requested
}

// PageWindow 按总数与(已钳制的)页长算出实际页码、页数与 OFFSET(P1 规格 §6 BrowseListings 第 6 条):
//
//	page_count = max(1, ceil(total / pageSize))
//	page       = min(max(page, 1), page_count, maxPage)   // maxPage=0 表示不设页码上限
//	offset     = (page - 1) × pageSize
func PageWindow(total uint64, page, pageSize, maxPage uint32) (clampedPage, pageCount uint32, offset uint64) {
	if pageSize == 0 {
		pageSize = 1
	}
	count := (total + uint64(pageSize) - 1) / uint64(pageSize)
	if count < 1 {
		count = 1
	}
	if count > math.MaxUint32 {
		count = math.MaxUint32
	}
	pageCount = uint32(count)

	clampedPage = max(page, 1)
	clampedPage = min(clampedPage, pageCount)
	if maxPage > 0 {
		clampedPage = min(clampedPage, maxPage)
	}
	offset = uint64(clampedPage-1) * uint64(pageSize)
	return clampedPage, pageCount, offset
}

// likeEscaper 用 '!' 作 LIKE 转义符,转义 '!' 自身与两个通配符。
// strings.Replacer 单遍、不回扫,已替换出的 '!' 不会被二次转义。
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// EscapeLike 把搜索词转成 LIKE 字面量,配合 SQL 里固定的 ESCAPE '!' 使用。
// 选 '!' 而不是 '\\':反斜杠在 MySQL 字符串字面量里的语义受 sql_mode NO_BACKSLASH_ESCAPES 影响,
// '!' 在任何 sql_mode 下都是普通字符。
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}

// ValidText 校验展示文本:合法 UTF-8、不含控制字符(含换行 / 制表)、字符数 ≤ maxRunes。空串合法。
// 控制字符会破坏客户端单行排版与日志行,且没有任何合法的展示用途。
func ValidText(s string, maxRunes int) bool {
	if !utf8.ValidString(s) {
		return false
	}
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
		n++
		if n > maxRunes {
			return false
		}
	}
	return true
}

// ValidIconKey 校验图标资源键:空串合法(客户端用类目默认图标);否则 ≤ MaxIconKeyLen 字节且只含 [a-z0-9_]。
func ValidIconKey(s string) bool {
	if len(s) > constants.MaxIconKeyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// NormalizeSearch 去掉首尾空白后校验搜索词。返回 ok=false 表示非法(超长 / 控制字符 / 非法 UTF-8);
// 合法时返回 trim 后的词,可能为空(= 不搜)。
func NormalizeSearch(raw string) (string, bool) {
	search := strings.TrimSpace(raw)
	if !ValidText(search, constants.MaxSearchRunes) {
		return "", false
	}
	return search, true
}

// ValidCategory 校验类目与子类:类目 ∈ 1..9,子类 ≤ 该类目上限(0 = 全部 / 无子类)。
func ValidCategory(category tradepb.ListingCategory, subcategory uint32) bool {
	limit, ok := constants.MaxSubcategory(category)
	return ok && subcategory <= limit
}

// ValidTab:只接受公示 / 寄售两个页签。
func ValidTab(tab tradepb.ListingTab) bool {
	return tab == tradepb.ListingTab_LISTING_TAB_PUBLIC_NOTICE || tab == tradepb.ListingTab_LISTING_TAB_ON_SALE
}

// ValidSection:只接受寄售 / 竞价两个分区(竞价在 P1 另回 TradeFeatureDisabled)。
func ValidSection(section tradepb.ListingSection) bool {
	return section == tradepb.ListingSection_LISTING_SECTION_CONSIGNMENT ||
		section == tradepb.ListingSection_LISTING_SECTION_AUCTION
}

// ValidSort:只接受 proto 里声明过的排序。
func ValidSort(sort tradepb.ListingSort) bool {
	switch sort {
	case tradepb.ListingSort_LISTING_SORT_DEFAULT,
		tradepb.ListingSort_LISTING_SORT_PRICE_ASC,
		tradepb.ListingSort_LISTING_SORT_PRICE_DESC,
		tradepb.ListingSort_LISTING_SORT_LEVEL_DESC,
		tradepb.ListingSort_LISTING_SORT_REMAINING_ASC:
		return true
	default:
		return false
	}
}

// ValidSeedRequest 校验 SeedListing 的全部输入(P1 规格 §6 SeedListing 第 2 条)。
func ValidSeedRequest(in *tradepb.SeedListingRequest) bool {
	switch {
	case in.GetSellerPlayerId() == 0:
		return false
	case !ValidCategory(in.GetCategory(), in.GetSubcategory()):
		return false
	case strings.TrimSpace(in.GetTitle()) == "" || !ValidText(in.GetTitle(), constants.MaxTitleRunes):
		return false
	case !ValidText(in.GetSummary(), constants.MaxSummaryRunes):
		return false
	case !ValidText(in.GetDescription(), constants.MaxDescriptionRunes):
		return false
	case !ValidIconKey(in.GetIconKey()):
		return false
	case in.GetLevel() > constants.MaxLevel:
		return false
	case in.GetPriceFen() == 0 || in.GetPriceFen() > constants.MaxPriceFen:
		return false
	case in.GetSaleDurationMs() == 0 || in.GetSaleDurationMs() > uint64(constants.MaxSaleDuration.Milliseconds()):
		return false
	case in.GetNoticeDurationMs() > uint64(constants.MaxNoticeDuration.Milliseconds()):
		return false
	}
	return true
}
