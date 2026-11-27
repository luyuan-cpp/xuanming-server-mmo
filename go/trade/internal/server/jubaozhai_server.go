// Package server 是 trade 的 gRPC 入口薄包装(照 chat / match 的 internal/server 模式):
// 每个 RPC 委托给 logic,不写业务。
package server

import (
	"context"

	"trade/internal/logic"

	tradepb "proto/trade"
)

// JubaozhaiServer 实现 tradepb.ClientPlayerJubaozhaiServer。
// 该 service 标了 OptionIsClientProtocolService=true:只接客户端经 gate → 路由服转发来的请求;
// 东西向 / 运维方法一律放 TradeAdmin,不许加到这个 service 上。
type JubaozhaiServer struct {
	tradepb.UnimplementedClientPlayerJubaozhaiServer
	deps logic.Deps
}

func (s *JubaozhaiServer) ListOfficialSkus(ctx context.Context, in *tradepb.ListOfficialSkusRequest) (*tradepb.ListOfficialSkusResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ListOfficialSkus(in)
}
func (s *JubaozhaiServer) CreateOfficialOrder(ctx context.Context, in *tradepb.CreateOfficialOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CreateOfficialOrder(in)
}

func NewJubaozhaiServer(deps logic.Deps) *JubaozhaiServer {
	return &JubaozhaiServer{deps: deps}
}

// BrowseListings 浏览公示 / 寄售列表。
func (s *JubaozhaiServer) BrowseListings(ctx context.Context, in *tradepb.BrowseListingsRequest) (*tradepb.BrowseListingsResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).BrowseListings(in)
}

// GetListingDetail 取单件商品详情。
func (s *JubaozhaiServer) GetListingDetail(ctx context.Context, in *tradepb.GetListingDetailRequest) (*tradepb.GetListingDetailResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetListingDetail(in)
}

// SetFavorite 收藏 / 取消收藏。
func (s *JubaozhaiServer) SetFavorite(ctx context.Context, in *tradepb.SetFavoriteRequest) (*tradepb.SetFavoriteResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).SetFavorite(in)
}

// GetMyShelf 取调用者自己的货架。
func (s *JubaozhaiServer) GetMyShelf(ctx context.Context, in *tradepb.GetMyShelfRequest) (*tradepb.GetMyShelfResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetMyShelf(in)
}

func (s *JubaozhaiServer) GetCommerceOrder(ctx context.Context, in *tradepb.GetCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetCommerceOrder(in)
}
func (s *JubaozhaiServer) CancelCommerceOrder(ctx context.Context, in *tradepb.CancelCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CancelCommerceOrder(in)
}
func (s *JubaozhaiServer) ListCommerceOrders(ctx context.Context, in *tradepb.ListCommerceOrdersRequest) (*tradepb.ListCommerceOrdersResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ListCommerceOrders(in)
}
func (s *JubaozhaiServer) CreatePlayerTrade(ctx context.Context, in *tradepb.CreatePlayerTradeRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CreatePlayerTrade(in)
}
func (s *JubaozhaiServer) ConfirmPlayerTrade(ctx context.Context, in *tradepb.ConfirmPlayerTradeRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ConfirmPlayerTrade(in)
}
func (s *JubaozhaiServer) CreateConsignment(ctx context.Context, in *tradepb.CreateConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CreateConsignment(in)
}
func (s *JubaozhaiServer) BuyConsignment(ctx context.Context, in *tradepb.BuyConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).BuyConsignment(in)
}
func (s *JubaozhaiServer) CancelConsignment(ctx context.Context, in *tradepb.CancelConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CancelConsignment(in)
}
func (s *JubaozhaiServer) PlaceMarketOrder(ctx context.Context, in *tradepb.PlaceMarketOrderRequest) (*tradepb.MarketOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).PlaceMarketOrder(in)
}
func (s *JubaozhaiServer) CancelMarketOrder(ctx context.Context, in *tradepb.CancelMarketOrderRequest) (*tradepb.MarketOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CancelMarketOrder(in)
}
func (s *JubaozhaiServer) ListMarketOrders(ctx context.Context, in *tradepb.ListMarketOrdersRequest) (*tradepb.ListMarketOrdersResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ListMarketOrders(in)
}
func (s *JubaozhaiServer) GetMarketDepth(ctx context.Context, in *tradepb.GetMarketDepthRequest) (*tradepb.GetMarketDepthResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetMarketDepth(in)
}
func (s *JubaozhaiServer) CreateAuctionLot(ctx context.Context, in *tradepb.CreateAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CreateAuctionLot(in)
}
func (s *JubaozhaiServer) PlaceAuctionBid(ctx context.Context, in *tradepb.PlaceAuctionBidRequest) (*tradepb.AuctionLotResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).PlaceAuctionBid(in)
}
func (s *JubaozhaiServer) GetAuctionLot(ctx context.Context, in *tradepb.GetAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetAuctionLot(in)
}
func (s *JubaozhaiServer) CancelAuctionLot(ctx context.Context, in *tradepb.CancelAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CancelAuctionLot(in)
}
func (s *JubaozhaiServer) BrowseCommerceOffers(ctx context.Context, in *tradepb.BrowseCommerceOffersRequest) (*tradepb.BrowseCommerceOffersResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).BrowseCommerceOffers(in)
}
func (s *JubaozhaiServer) ListTradeMarkets(ctx context.Context, in *tradepb.ListTradeMarketsRequest) (*tradepb.ListTradeMarketsResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ListTradeMarkets(in)
}
func (s *JubaozhaiServer) GetSellerCredit(ctx context.Context, in *tradepb.GetSellerCreditRequest) (*tradepb.SellerCreditResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetSellerCredit(in)
}
func (s *JubaozhaiServer) SubmitSellerReview(ctx context.Context, in *tradepb.SubmitSellerReviewRequest) (*tradepb.SubmitSellerReviewResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).SubmitSellerReview(in)
}
func (s *JubaozhaiServer) GetMarketReferencePrice(ctx context.Context, in *tradepb.GetMarketReferencePriceRequest) (*tradepb.GetMarketReferencePriceResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetMarketReferencePrice(in)
}
func (s *JubaozhaiServer) GetPlayerTradeQuota(ctx context.Context, in *tradepb.GetPlayerTradeQuotaRequest) (*tradepb.GetPlayerTradeQuotaResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).GetPlayerTradeQuota(in)
}

// CreateWantedSkillBookOrder / FulfillWantedSkillBookOrder / ListWantedSkillBookOrders:悬赏求书求购单(kind 7)的发榜、揭榜与悬赏榜。
func (s *JubaozhaiServer) CreateWantedSkillBookOrder(ctx context.Context, in *tradepb.CreateWantedSkillBookOrderRequest) (*tradepb.WantedSkillBookOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).CreateWantedSkillBookOrder(in)
}
func (s *JubaozhaiServer) FulfillWantedSkillBookOrder(ctx context.Context, in *tradepb.FulfillWantedSkillBookOrderRequest) (*tradepb.WantedSkillBookOrderResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).FulfillWantedSkillBookOrder(in)
}
func (s *JubaozhaiServer) ListWantedSkillBookOrders(ctx context.Context, in *tradepb.ListWantedSkillBookOrdersRequest) (*tradepb.ListWantedSkillBookOrdersResponse, error) {
	return logic.NewJubaozhaiLogic(ctx, s.deps).ListWantedSkillBookOrders(in)
}
