// Package logic 是聚宝斋 P1 的业务逻辑:客户端 ClientPlayerJubaozhai 的四个方法与内部 TradeAdmin.SeedListing。
//
// 错误语义(P1-9):业务结果一律 in-band —— `return &Resp{ErrorMessage: &TipInfoMessage{Id: code}}, nil`。
// 路由服 / gate 的回包桥接只认成功响应,gRPC 错误到了客户端只剩信封级失败;存储 / data_service / 发号
// 故障用 fault 码 kServiceUnavailable 表达并打错误日志,由 serverbase 记成 rpc_inband_fault 告警。
// 唯一例外是 SeedListing 在非 dev/test 下回 gRPC PermissionDenied:调用方是 robot / 内部工具,不是客户端。
//
// 身份只取会话(session.ClientPlayerID),请求体里刻意没有 player_id。
// 时间只在每个请求里取一次(Deps.Now),阶段推导与 server_now_ms 用同一个值,避免一次响应前后矛盾。
//
// 超时:每个方法入口先套整请求业务预算(config.Config.RequestBudget() = Timeout − InBandReplyReserve),
// 本次请求的 home_zone 查询、MySQL、发号全部用这一个 ctx。单次调用的上限(constants.HomeZoneLookupTimeout /
// StoreOpTimeout)只限单次,串行几次就会越过 go-zero 服务端超时拦截器;拦截器一旦先到,回的是 gRPC
// DeadlineExceeded,in-band 故障码丢失(P1-9)。
//
// P1 不接合服闸、不读 SharedRedis、不移动任何资产(P1-10)。
package logic

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"
	"trade/internal/svc"

	base "proto/common/base"
	tradepb "proto/trade"

	"shared/idsegment"

	"github.com/zeromicro/go-zero/core/logx"
)

// Deps 是逻辑层的全部外部依赖,进程启动时由 NewDeps 组装一次;单测直接构造并注入 fake。
type Deps struct {
	Config     config.Config
	Store      data.ListingStore
	HomeZones  HomeZoneLookup
	ListingIDs idsegment.Source
	// Now 是时间源,生产为 time.Now;单测注入固定时钟验证公示 / 寄售边界与 server_now_ms。
	Now func() time.Time
}

// NewDeps 用 ServiceContext 组装生产依赖。
func NewDeps(svcCtx *svc.ServiceContext) Deps {
	var ids idsegment.Source
	if svcCtx.ListingIDSegment != nil { // 别把 nil *Client 装进非 nil 接口
		ids = svcCtx.ListingIDSegment
	}
	return Deps{
		Config:     svcCtx.Config,
		Store:      data.NewListingRepo(svcCtx.DB, constants.StoreOpTimeout),
		HomeZones:  NewDataServiceHomeZone(svcCtx.DataServiceClient, constants.HomeZoneLookupTimeout),
		ListingIDs: ids,
		Now:        time.Now,
	}
}

// JubaozhaiLogic 是一次客户端请求的逻辑上下文(照 go-zero logic 惯例,每请求一个)。
type JubaozhaiLogic struct {
	logx.Logger
	ctx  context.Context
	deps Deps
}

func NewJubaozhaiLogic(ctx context.Context, deps Deps) *JubaozhaiLogic {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &JubaozhaiLogic{Logger: logx.WithContext(ctx), ctx: ctx, deps: deps}
}

// requestContext 给一次请求套上整请求业务预算;父 ctx 更早到期时以父 ctx 为准。
func requestContext(ctx context.Context, c config.Config) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.RequestBudget())
}

// withRequestBudget 返回带整请求预算 ctx 的副本,cancel 必须 defer。
// 返回副本而不是改 l 本身:同一个 JubaozhaiLogic 连续调多个方法时,上一个方法 cancel 掉的 ctx 不会漏给下一个。
func (l *JubaozhaiLogic) withRequestBudget() (*JubaozhaiLogic, context.CancelFunc) {
	ctx, cancel := requestContext(l.ctx, l.deps.Config)
	scoped := *l
	scoped.ctx = ctx
	return &scoped, cancel
}

// BrowseListings 浏览公示 / 寄售列表(P1 规格 §6 BrowseListings)。
//
// 顺序:会话 → 全部纯校验 → 竞价拒绝 → 取一次 now → 按 scope 定分区 → COUNT → 钳制页码 → 分页查询 → 收藏标记。
// 纯校验在前:被拒的请求不查 home_zone、不碰库。
func (l *JubaozhaiLogic) BrowseListings(in *tradepb.BrowseListingsRequest) (*tradepb.BrowseListingsResponse, error) {
	const method = "BrowseListings"
	l, cancel := l.withRequestBudget()
	defer cancel()
	reject := func(code uint32) (*tradepb.BrowseListingsResponse, error) {
		return &tradepb.BrowseListingsResponse{ErrorMessage: tipOf(code)}, nil
	}

	caller, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return reject(constants.ErrInvalidParameter)
	}
	search, searchOK := NormalizeSearch(in.GetSearch())
	if !searchOK || !ValidTab(in.GetTab()) || !ValidSection(in.GetSection()) ||
		!ValidCategory(in.GetCategory(), in.GetSubcategory()) || !ValidSort(in.GetSort()) {
		return reject(constants.ErrInvalidParameter)
	}
	if in.GetSection() == tradepb.ListingSection_LISTING_SECTION_AUCTION {
		return reject(constants.ErrFeatureDisabled)
	}

	market := l.deps.Config.Market
	scope := market.ScopeEnum()
	pageSize := ClampPageSize(in.GetPageSize(), market.DefaultPageSize, market.MaxPageSize)
	nowMs := nowMillis(l.deps.Now)

	q := data.ListingQuery{
		Tab:         in.GetTab(),
		Category:    in.GetCategory(),
		Subcategory: in.GetSubcategory(),
		Sort:        in.GetSort(),
		NowMs:       nowMs,
	}
	switch scope {
	case tradepb.MarketScope_MARKET_SCOPE_ZONE:
		// zone 范围:分区只认调用者 home_zone,客户端传的 zone_filter 一律忽略(P1-7)。
		zone, code := resolveHomeZone(l.ctx, l.Logger, l.deps.HomeZones, caller, method)
		if code != 0 {
			return reject(code)
		}
		q.MarketZone = zone
	case tradepb.MarketScope_MARKET_SCOPE_GLOBAL:
		q.MarketZone = in.GetZoneFilter() // 0 = 全部区
	default:
		// config.Validate 已保证不会走到这里;万一走到,按故障拒绝而不是退化成全服可见。
		l.Errorf("[trade] %s: Market.Scope=%q 非法,拒绝请求 player=%d", method, market.Scope, caller)
		return reject(constants.ErrServiceUnavailable)
	}
	if search != "" {
		q.TitleLikePattern = "%" + EscapeLike(search) + "%"
		// 纯数字的搜索词同时按商品编号精确匹配;0 不是合法编号,不参与。
		if id, err := strconv.ParseUint(search, 10, 64); err == nil {
			q.SearchListingID = id
		}
	}
	if in.GetFavoritesOnly() {
		q.FavoritesOf = caller
	}

	total, err := l.deps.Store.CountListings(l.ctx, q)
	if err != nil {
		return reject(l.storeFault(method, "CountListings", caller, err))
	}
	page, pageCount, offset := PageWindow(total, in.GetPage(), pageSize, market.MaxPage)
	recs, err := l.deps.Store.QueryListings(l.ctx, q, offset, uint64(pageSize))
	if err != nil {
		return reject(l.storeFault(method, "QueryListings", caller, err))
	}
	favorites, err := favoriteSet(l.ctx, l.deps.Store, caller, recs)
	if err != nil {
		return reject(l.storeFault(method, "FavoriteIDs", caller, err))
	}

	return &tradepb.BrowseListingsResponse{
		Listings:    toSummaries(recs, nowMs, caller, favorites),
		TotalCount:  clampUint32(total),
		Page:        page,
		PageSize:    pageSize,
		PageCount:   pageCount,
		MarketScope: scope,
		ServerNowMs: nowMs,
	}, nil
}

// GetMyShelf 返回调用者自己上架的商品(任意状态,新上架在前)。不查 home_zone:自己的商品不受市场范围约束。
func (l *JubaozhaiLogic) GetMyShelf(in *tradepb.GetMyShelfRequest) (*tradepb.GetMyShelfResponse, error) {
	const method = "GetMyShelf"
	l, cancel := l.withRequestBudget()
	defer cancel()
	reject := func(code uint32) (*tradepb.GetMyShelfResponse, error) {
		return &tradepb.GetMyShelfResponse{ErrorMessage: tipOf(code)}, nil
	}

	caller, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return reject(constants.ErrInvalidParameter)
	}
	market := l.deps.Config.Market
	pageSize := ClampPageSize(in.GetPageSize(), market.DefaultPageSize, market.MaxPageSize)
	nowMs := nowMillis(l.deps.Now)

	total, err := l.deps.Store.CountSellerListings(l.ctx, caller)
	if err != nil {
		return reject(l.storeFault(method, "CountSellerListings", caller, err))
	}
	page, pageCount, offset := PageWindow(total, in.GetPage(), pageSize, market.MaxPage)
	recs, err := l.deps.Store.QuerySellerListings(l.ctx, caller, offset, uint64(pageSize))
	if err != nil {
		return reject(l.storeFault(method, "QuerySellerListings", caller, err))
	}
	favorites, err := favoriteSet(l.ctx, l.deps.Store, caller, recs)
	if err != nil {
		return reject(l.storeFault(method, "FavoriteIDs", caller, err))
	}

	return &tradepb.GetMyShelfResponse{
		Listings:    toSummaries(recs, nowMs, caller, favorites),
		TotalCount:  clampUint32(total),
		Page:        page,
		PageSize:    pageSize,
		PageCount:   pageCount,
		ServerNowMs: nowMs,
	}, nil
}

// GetListingDetail 返回单件商品详情。不存在、已结束、或 zone 范围下属于别区 → TradeListingNotFound;
// 卖家本人始终可见。
func (l *JubaozhaiLogic) GetListingDetail(in *tradepb.GetListingDetailRequest) (*tradepb.GetListingDetailResponse, error) {
	const method = "GetListingDetail"
	l, cancel := l.withRequestBudget()
	defer cancel()
	reject := func(code uint32) (*tradepb.GetListingDetailResponse, error) {
		return &tradepb.GetListingDetailResponse{ErrorMessage: tipOf(code)}, nil
	}

	caller, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return reject(constants.ErrInvalidParameter)
	}
	listingID := in.GetListingId()
	if listingID == 0 {
		return reject(constants.ErrInvalidParameter)
	}
	nowMs := nowMillis(l.deps.Now)

	rec, code := l.loadVisibleListing(method, caller, listingID, nowMs)
	if code != 0 {
		return reject(code)
	}
	favorite, err := l.deps.Store.FavoriteExists(l.ctx, caller, listingID)
	if err != nil {
		return reject(l.storeFault(method, "FavoriteExists", caller, err))
	}

	return &tradepb.GetListingDetailResponse{
		Detail: &tradepb.ListingDetail{
			Summary:     toSummary(rec, nowMs, caller, favorite),
			Description: rec.GetDescription(),
		},
		ServerNowMs: nowMs,
	}, nil
}

// SetFavorite 收藏 / 取消收藏。
//
//   - 取消:直接 DELETE,幂等,不查商品 —— 商品已下架 / 已结束的收藏也必须能删掉。
//   - 收藏:商品必须对调用者可见(规则同详情);已收藏直接回 true;否则检查上限后 INSERT IGNORE。
//
// 上限是**软上限**:CountFavorites 与 InsertFavorite 之间没有锁,同一玩家的并发收藏请求最多能超出
// "在途请求数"条。可接受的依据(AGENTS.md §11.3):收藏不涉及资产、不影响他人,上限只为防止单个玩家
// 无限堆行;为它加行锁或计数表会让每次收藏多一次写放大,得不偿失。gate 的 MessageLimiter 进一步
// 压低了同一会话的并发度。
func (l *JubaozhaiLogic) SetFavorite(in *tradepb.SetFavoriteRequest) (*tradepb.SetFavoriteResponse, error) {
	const method = "SetFavorite"
	l, cancel := l.withRequestBudget()
	defer cancel()
	listingID := in.GetListingId()
	reject := func(code uint32) (*tradepb.SetFavoriteResponse, error) {
		return &tradepb.SetFavoriteResponse{ErrorMessage: tipOf(code), ListingId: listingID}, nil
	}
	accept := func(favorite bool) (*tradepb.SetFavoriteResponse, error) {
		return &tradepb.SetFavoriteResponse{ListingId: listingID, Favorite: favorite}, nil
	}

	caller, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return reject(constants.ErrInvalidParameter)
	}
	if listingID == 0 {
		return reject(constants.ErrInvalidParameter)
	}

	if !in.GetFavorite() {
		if err := l.deps.Store.DeleteFavorite(l.ctx, caller, listingID); err != nil {
			return reject(l.storeFault(method, "DeleteFavorite", caller, err))
		}
		return accept(false)
	}

	nowMs := nowMillis(l.deps.Now)
	if _, code := l.loadVisibleListing(method, caller, listingID, nowMs); code != 0 {
		return reject(code)
	}
	exists, err := l.deps.Store.FavoriteExists(l.ctx, caller, listingID)
	if err != nil {
		return reject(l.storeFault(method, "FavoriteExists", caller, err))
	}
	if exists {
		return accept(true)
	}
	count, err := l.deps.Store.CountFavorites(l.ctx, caller)
	if err != nil {
		return reject(l.storeFault(method, "CountFavorites", caller, err))
	}
	if count >= uint64(l.deps.Config.Market.MaxFavoritesPerPlayer) {
		return reject(constants.ErrFavoriteLimitReached)
	}
	if err := l.deps.Store.InsertFavorite(l.ctx, &tradepb.TradeFavoriteRecord{
		PlayerId:  caller,
		ListingId: listingID,
		CreatedMs: nowMs,
	}); err != nil {
		return reject(l.storeFault(method, "InsertFavorite", caller, err))
	}
	return accept(true)
}

// loadVisibleListing 取商品并按详情可见性判定:卖家本人,或 VisibleToBuyer。
// 返回的码 ≠ 0 时应原样 in-band 返回。zone 范围下非卖家才查 home_zone。
func (l *JubaozhaiLogic) loadVisibleListing(method string, caller, listingID, nowMs uint64) (*tradepb.TradeListingRecord, uint32) {
	rec, err := l.deps.Store.GetListing(l.ctx, listingID)
	if errors.Is(err, data.ErrListingNotFound) {
		return nil, constants.ErrListingNotFound
	}
	if err != nil {
		return nil, l.storeFault(method, "GetListing", caller, err)
	}
	if rec.GetSellerPlayerId() == caller {
		return rec, 0
	}

	scope := l.deps.Config.Market.ScopeEnum()
	var callerHomeZone uint32
	if scope == tradepb.MarketScope_MARKET_SCOPE_ZONE {
		zone, code := resolveHomeZone(l.ctx, l.Logger, l.deps.HomeZones, caller, method)
		if code != 0 {
			return nil, code
		}
		callerHomeZone = zone
	}
	if !VisibleToBuyer(rec, nowMs, scope, callerHomeZone) {
		// 别区 / 已结束的商品对调用者表现为"不存在",不泄露别区有哪些商品。
		return nil, constants.ErrListingNotFound
	}
	return rec, 0
}

// storeFault 记一条存储故障日志并返回 fault 码。player_id 只进日志,不进指标(AGENTS.md §9)。
func (l *JubaozhaiLogic) storeFault(method, op string, caller uint64, err error) uint32 {
	l.Errorf("[trade] %s: %s 失败 player=%d: %v", method, op, caller, err)
	return constants.ErrServiceUnavailable
}

// resolveHomeZone 查玩家 home_zone。返回码 ≠ 0 时应原样 in-band 返回:
// 未映射 → TradeHomeZoneUnknown(业务拒绝);查询失败 / 未接线 → kServiceUnavailable(故障,打错误日志)。
func resolveHomeZone(ctx context.Context, logger logx.Logger, lookup HomeZoneLookup, playerID uint64, method string) (uint32, uint32) {
	if lookup == nil {
		logger.Errorf("[trade] %s: home_zone 查询未接线 player=%d", method, playerID)
		return 0, constants.ErrServiceUnavailable
	}
	zone, err := lookup.HomeZone(ctx, playerID)
	if err != nil {
		logger.Errorf("[trade] %s: 查询 home_zone 失败 player=%d: %v", method, playerID, err)
		return 0, constants.ErrServiceUnavailable
	}
	if zone == 0 {
		logger.Infof("[trade] %s: player=%d 没有 home_zone 映射", method, playerID)
		return 0, constants.ErrHomeZoneUnknown
	}
	return zone, 0
}

// favoriteSet 批量查本页商品里调用者收藏过的;空页不查库。
func favoriteSet(ctx context.Context, store data.ListingStore, caller uint64, recs []*tradepb.TradeListingRecord) (map[uint64]bool, error) {
	if len(recs) == 0 {
		return map[uint64]bool{}, nil
	}
	ids := make([]uint64, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.GetListingId())
	}
	return store.FavoriteIDs(ctx, caller, ids)
}

func toSummaries(recs []*tradepb.TradeListingRecord, nowMs, caller uint64, favorites map[uint64]bool) []*tradepb.ListingSummary {
	out := make([]*tradepb.ListingSummary, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toSummary(rec, nowMs, caller, favorites[rec.GetListingId()]))
	}
	return out
}

// toSummary 把存储行转成客户端可见的摘要。刻意不带 seller_player_id / seller_account(设计 §9):
// 客户端只需要知道"是不是我",不需要知道卖家是谁。
func toSummary(rec *tradepb.TradeListingRecord, nowMs, caller uint64, favorite bool) *tradepb.ListingSummary {
	return &tradepb.ListingSummary{
		ListingId:   rec.GetListingId(),
		Category:    rec.GetCategory(),
		Subcategory: rec.GetSubcategory(),
		Title:       rec.GetTitle(),
		Level:       rec.GetLevel(),
		PriceFen:    rec.GetPriceFen(),
		Phase:       Phase(rec, nowMs),
		NoticeEndMs: rec.GetNoticeEndMs(),
		SaleEndMs:   rec.GetSaleEndMs(),
		MarketZone:  rec.GetMarketZone(),
		IsFavorite:  favorite,
		IsMine:      rec.GetSellerPlayerId() == caller,
		Summary:     rec.GetSummary(),
		IconKey:     rec.GetIconKey(),
	}
}

func tipOf(code uint32) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: code}
}

// nowMillis 取一次时间转成 Unix 毫秒;时钟早于 1970 时按 0(不会发生,只防 uint64 回绕)。
func nowMillis(now func() time.Time) uint64 {
	ms := now().UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

func clampUint32(v uint64) uint32 {
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}
