package server

import (
	"context"

	"trade/internal/logic"

	tradepb "proto/trade"
)

// AdminServer 实现 tradepb.TradeAdminServer(内部服务,不标客户端协议)。
// 客户端来源的调用在会话拦截器就被 PermissionDenied;robot / 运维工具不带会话直连到达。
type AdminServer struct {
	tradepb.UnimplementedTradeAdminServer
	deps logic.Deps
}

func NewAdminServer(deps logic.Deps) *AdminServer {
	return &AdminServer{deps: deps}
}

// SeedListing 造一条已上架商品(只在 Mode=dev|test 可用)。
func (s *AdminServer) SeedListing(ctx context.Context, in *tradepb.SeedListingRequest) (*tradepb.SeedListingResponse, error) {
	return logic.NewAdminLogic(ctx, s.deps).SeedListing(in)
}
