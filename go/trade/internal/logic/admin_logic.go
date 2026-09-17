package logic

import (
	"context"
	"time"

	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/svc"

	tradepb "proto/trade"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminLogic 是一次 TradeAdmin 内部调用的逻辑上下文。
type AdminLogic struct {
	logx.Logger
	ctx  context.Context
	deps Deps
}

func NewAdminLogic(ctx context.Context, deps Deps) *AdminLogic {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &AdminLogic{Logger: logx.WithContext(ctx), ctx: ctx, deps: deps}
}

// withRequestBudget 同 JubaozhaiLogic.withRequestBudget:home_zone 查询、发号、插入共用一个整请求预算,
// 依赖变慢时在服务端 Timeout 之前 in-band 回 kServiceUnavailable。
func (l *AdminLogic) withRequestBudget() (*AdminLogic, context.CancelFunc) {
	ctx, cancel := requestContext(l.ctx, l.deps.Config)
	scoped := *l
	scoped.ctx = ctx
	return &scoped, cancel
}

// SeedListing 造一条已上架商品,只用于本地联调与 robot 冒烟;不移动任何资产(P1-5、P1-10)。
//
// 防线:
//  1. 会话拦截器:带客户端会话调用本方法 → PermissionDenied(到不了这里);
//  2. 本方法:Mode ∉ {dev, test} → gRPC PermissionDenied。放在方法里而不是"只在 dev 注册 service":
//     TradeAdmin 以后还要放生产可用的运维方法。
//
// market_zone 不来自请求(proto 里刻意没有该字段):按卖家查 home_zone,与正式上架同一条路径,
// 所以 robot 用两个 zone 的卖家就能同时验收 zone / global 两种范围。
// 本方法不幂等:每次调用都发新 listing_id(dev 工具,调用方按 nonce 标题找自己的商品)。
func (l *AdminLogic) SeedListing(in *tradepb.SeedListingRequest) (*tradepb.SeedListingResponse, error) {
	const method = "SeedListing"
	l, cancel := l.withRequestBudget()
	defer cancel()
	mode := l.deps.Config.Mode
	if !config.IsRelaxedMode(mode) {
		svc.ObserveSeedListing(svc.ResultRejected)
		l.Errorf("[trade] 拒绝 SeedListing:Mode=%q 不是 dev/test", mode)
		return nil, status.Errorf(codes.PermissionDenied, "SeedListing is only available in Mode=dev|test (current %q)", mode)
	}
	reject := func(code uint32, result string) (*tradepb.SeedListingResponse, error) {
		svc.ObserveSeedListing(result)
		return &tradepb.SeedListingResponse{ErrorMessage: tipOf(code)}, nil
	}

	if !ValidSeedRequest(in) {
		return reject(constants.ErrInvalidParameter, svc.ResultRejected)
	}
	seller := in.GetSellerPlayerId()

	homeZone, code := resolveHomeZone(l.ctx, l.Logger, l.deps.HomeZones, seller, method)
	if code != 0 {
		result := svc.ResultError
		if code == constants.ErrHomeZoneUnknown {
			result = svc.ResultRejected
		}
		return reject(code, result)
	}

	if l.deps.ListingIDs == nil {
		l.Errorf("[trade] %s: listing_id 号段未接线 seller=%d", method, seller)
		return reject(constants.ErrServiceUnavailable, svc.ResultError)
	}
	listingID, err := l.deps.ListingIDs.Next(l.ctx)
	if err != nil {
		l.Errorf("[trade] %s: 领 listing_id 失败 seller=%d: %v", method, seller, err)
		return reject(constants.ErrServiceUnavailable, svc.ResultError)
	}
	if listingID == 0 {
		// idsegment 保证不返回 0;真出现就是发号源 bug,绝不能写进主键。
		l.Errorf("[trade] %s: 号段返回了 listing_id=0 seller=%d", method, seller)
		return reject(constants.ErrServiceUnavailable, svc.ResultError)
	}

	nowMs := nowMillis(l.deps.Now)
	noticeEndMs := nowMs + in.GetNoticeDurationMs()
	rec := &tradepb.TradeListingRecord{
		ListingId:           listingID,
		SellerPlayerId:      seller,
		SellerAccount:       "", // P3 下单判"同账号不能自买"时才需要;种子不读 SharedRedis(P1-10)
		MarketZone:          homeZone,
		SellerZoneAtListing: homeZone,
		Category:            in.GetCategory(),
		Subcategory:         in.GetSubcategory(),
		Title:               in.GetTitle(),
		Level:               in.GetLevel(),
		PriceFen:            in.GetPriceFen(),
		Status:              tradepb.ListingStatus_LISTING_STATUS_LISTED,
		Summary:             in.GetSummary(),
		Description:         in.GetDescription(),
		IconKey:             in.GetIconKey(),
		NoticeEndMs:         noticeEndMs,
		SaleEndMs:           noticeEndMs + in.GetSaleDurationMs(),
		CreatedMs:           nowMs,
		UpdatedMs:           nowMs,
		Version:             0,
	}
	if err := l.deps.Store.InsertListing(l.ctx, rec); err != nil {
		l.Errorf("[trade] %s: InsertListing 失败 listing_id=%d seller=%d: %v", method, listingID, seller, err)
		return reject(constants.ErrServiceUnavailable, svc.ResultError)
	}

	svc.ObserveSeedListing(svc.ResultOK)
	l.Infof("[trade] %s 成功 listing_id=%d seller=%d market_zone=%d category=%d notice_end_ms=%d sale_end_ms=%d",
		method, listingID, seller, homeZone, int32(rec.GetCategory()), rec.GetNoticeEndMs(), rec.GetSaleEndMs())
	return &tradepb.SeedListingResponse{ListingId: listingID, MarketZone: homeZone}, nil
}
