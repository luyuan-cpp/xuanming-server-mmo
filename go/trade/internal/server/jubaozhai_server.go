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
