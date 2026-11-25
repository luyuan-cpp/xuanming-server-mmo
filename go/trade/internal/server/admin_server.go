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

// ListManualContracts 列转人工的合同(人工处置入口,只认 serviceauth 调用方 ops-commerce;入口没开回 PermissionDenied)。
func (s *AdminServer) ListManualContracts(ctx context.Context, in *tradepb.ListManualContractsRequest) (*tradepb.ListManualContractsResponse, error) {
	return s.deps.ManualOps.ListManualContracts(ctx, in)
}

// ResolveManualContract 对一张转人工的合同做 SHOW / DRY_RUN / EXECUTE(设计 jubaozhai-market.md §15.7、§15.7.1)。
func (s *AdminServer) ResolveManualContract(ctx context.Context, in *tradepb.ResolveManualContractRequest) (*tradepb.ResolveManualContractResponse, error) {
	return s.deps.ManualOps.ResolveManualContract(ctx, in)
}

// RecordSellerEnvironment 上报卖家登录/交易环境信号（仅换IP不扣分，异地陌生新设备清0正面信用并保留违约黑历史）。
func (s *AdminServer) RecordSellerEnvironment(ctx context.Context, in *tradepb.RecordSellerEnvironmentRequest) (*tradepb.RecordSellerEnvironmentResponse, error) {
	return logic.NewAdminLogic(ctx, s.deps).RecordSellerEnvironment(in)
}

// RecordSellerViolation 记录经核实的卖家违约黑历史（按 case_id 幂等）。
func (s *AdminServer) RecordSellerViolation(ctx context.Context, in *tradepb.RecordSellerViolationRequest) (*tradepb.RecordSellerViolationResponse, error) {
	return logic.NewAdminLogic(ctx, s.deps).RecordSellerViolation(in)
}
