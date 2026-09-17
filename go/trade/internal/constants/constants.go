// Package constants 是 trade 写进 TipInfoMessage.id 的 tip 码、输入限值与子类编码上限表。
//
// 码值一律引用导表器生成的枚举(AGENTS.md §7.5),不许手写数字;constants_test.go 的
// TestNoHandWrittenTipCodes 机械守住这条。trade 同时引用两条码轴前缀:
//   - TradeError_:Tip.xlsx 的 //trade_error base=20000 段,聚宝斋专用业务码;
//   - CommonError_:无会话 / 参数非法(kInvalidParameter)与存储、依赖故障(kServiceUnavailable),
//     与 chat / 路由服同一口径(P1-9)。
package constants

import (
	"time"

	tradepb "proto/trade"

	"shared/generated/pb/table"
	"shared/serverbase"
)

const (
	// ErrInvalidParameter:取不到会话、枚举值未知、子类越界、文本超长或含控制字符 / 非法 UTF-8。
	// 无会话刻意不用 kPlayerNotFoundInSession:那是 fault 码,会把"客户端没带身份"刷成故障告警。
	ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)

	// ErrServiceUnavailable:MySQL / data_service / 号段故障。Tip.xlsx fault 列为 1,
	// serverbase 记成 rpc_inband_fault 并告警 —— **只用于真故障**,业务拒绝不许复用。
	ErrServiceUnavailable = uint32(table.CommonError_kServiceUnavailable)

	// ErrListingNotFound:商品不存在,或对调用者不可见(已结束、别区商品在 zone 范围下)。
	// 别区商品表现为"不存在"而不是专用码:不向客户端泄露别区有哪些商品。
	ErrListingNotFound = uint32(table.TradeError_kTradeListingNotFound)

	// ErrHomeZoneUnknown:data_service 映射里没有这名玩家的 home_zone(数据状态,不是故障)。
	ErrHomeZoneUnknown = uint32(table.TradeError_kTradeHomeZoneUnknown)

	// ErrFavoriteLimitReached:收藏数已达 Market.MaxFavoritesPerPlayer。
	ErrFavoriteLimitReached = uint32(table.TradeError_kTradeFavoriteLimitReached)

	// ErrFeatureDisabled:P1 未开放的功能(竞价分区 AUCTION)。
	ErrFeatureDisabled = uint32(table.TradeError_kTradeFeatureDisabled)
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
// 定性全部来自 Tip.xlsx 的 fault 列(生成到 shared/generated/tip.Faults);
// **不要**在这里包本地 map 去"微调" —— 要改分类,改表。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}

// 输入限值(设计 §8/§9;P1 规格 §6)。按 rune 计的是展示文本,按字节计的是资源键。
const (
	// MaxSearchRunes:BrowseListings.search trim 后的最大字符数。
	MaxSearchRunes = 64
	// MaxTitleRunes:商品标题最大字符数(标题会进 LIKE 搜索,不进索引)。
	MaxTitleRunes = 64
	// MaxSummaryRunes:列表"信息"列摘要最大字符数。
	MaxSummaryRunes = 128
	// MaxDescriptionRunes:详情描述最大字符数。
	MaxDescriptionRunes = 512
	// MaxIconKeyLen:客户端图标资源键最大字节数,字符集 ^[a-z0-9_]+$。
	MaxIconKeyLen = 64
	// MaxLevel:商品等级上限。
	MaxLevel = 1000
	// MaxPriceFen:单价上限(分),即 1 亿元。防止溢出与明显的误操作。
	MaxPriceFen uint64 = 10_000_000_000
	// MaxNoticeDuration:公示期上限。
	MaxNoticeDuration = 30 * 24 * time.Hour
	// MaxSaleDuration:寄售期上限。
	MaxSaleDuration = 90 * 24 * time.Hour
	// HomeZoneLookupTimeout:**单次** BatchGetPlayerHomeZone 的上限(data_service 一次 Redis 往返,
	// 与 guild / login 单查同值)。
	HomeZoneLookupTimeout = 1500 * time.Millisecond
	// StoreOpTimeout:**单次** MySQL 调用的上限;单条超时即按故障返回。
	//
	// 这两个值只限单次调用,不保证整请求落在服务端 Timeout 内:串行 I/O 最坏可达
	// 浏览 1500+3×2000=7500ms、收藏 1500+4×2000=9500ms,远超 Timeout 4000ms。整请求上限由
	// config.Config.RequestBudget()(= Timeout − InBandReplyReserve)封顶,逻辑层每个方法入口
	// 套一次;单次上限与整请求预算取先到者。
	StoreOpTimeout = 2000 * time.Millisecond
)

// MaxSubcategory 返回类目的子类编码上限(0 = 该类目没有子类),ok=false 表示类目本身非法。
//
// 编码是服务端与客户端的共同契约(P1 规格 §4):k(1 起)= 客户端 JubaozhaiCatalog.SubcategoriesFor(类目)
// 的第 k 个标签。客户端改标签顺序或数量时,这张表必须同步改,且已上架商品的子类不会自动迁移。
func MaxSubcategory(category tradepb.ListingCategory) (limit uint32, ok bool) {
	switch category {
	case tradepb.ListingCategory_LISTING_CATEGORY_CHARACTER:
		return 4, true // 破军 玄霄 逐风 丹心(门派)
	case tradepb.ListingCategory_LISTING_CATEGORY_PET:
		return 6, true // 普通 灵兽 变异 神兽 元灵 其他
	case tradepb.ListingCategory_LISTING_CATEGORY_WEAPON:
		return 5, true // 枪 爪 剑 扇 锤
	case tradepb.ListingCategory_LISTING_CATEGORY_ARMOR:
		return 5, true // 男帽 女帽 男衣 女衣 鞋子
	case tradepb.ListingCategory_LISTING_CATEGORY_SET,
		tradepb.ListingCategory_LISTING_CATEGORY_TREASURE,
		tradepb.ListingCategory_LISTING_CATEGORY_JEWELRY,
		tradepb.ListingCategory_LISTING_CATEGORY_CURRENCY:
		return 0, true
	case tradepb.ListingCategory_LISTING_CATEGORY_SUMMONING_ORDER:
		return 2, true // 神兽召唤令 元灵召唤令
	default:
		return 0, false
	}
}
