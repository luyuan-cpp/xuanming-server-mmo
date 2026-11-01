package server

import (
	"context"
	"errors"

	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *DataServiceServer) admitRoleTrade(ctx context.Context, method string, in proto.Message) error {
	if s.svcCtx.AccountRoleTrades == nil || s.svcCtx.AccountRoleTradeAuth == nil {
		return status.Error(codes.Unavailable, "角色交易目录未就绪")
	}
	caller, err := s.svcCtx.AccountRoleTradeAuth.Verify(ctx, accountdirectory.MethodPrefix+method, in)
	if err != nil || caller != "trade" {
		return status.Error(codes.PermissionDenied, "角色交易调用证明无效")
	}
	return nil
}
func (s *DataServiceServer) BeginAccountRoleTrade(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if err := s.admitRoleTrade(ctx, "BeginAccountRoleTrade", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountRoleTrades.Begin(ctx, in)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}
func (s *DataServiceServer) ConfirmAccountRoleTradeFreeze(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if err := s.admitRoleTrade(ctx, "ConfirmAccountRoleTradeFreeze", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountRoleTrades.Confirm(ctx, in)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}
func (s *DataServiceServer) CancelAccountRoleTrade(ctx context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if err := s.admitRoleTrade(ctx, "CancelAccountRoleTrade", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountRoleTrades.Cancel(ctx, in)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}
func (s *DataServiceServer) TransferAccountRole(ctx context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	if err := s.admitRoleTrade(ctx, "TransferAccountRole", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountRoleTrades.Transfer(ctx, in)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}

// CheckAccountRoleSlot 只读,结论带内回(规格 §5.4)。读失败回 Unavailable 的话 PublicationAdmission 会把排空证明打成 Hold。
func (s *DataServiceServer) CheckAccountRoleSlot(ctx context.Context, in *dspb.CheckAccountRoleSlotRequest) (*dspb.CheckAccountRoleSlotResponse, error) {
	if err := s.admitRoleTrade(ctx, "CheckAccountRoleSlot", in); err != nil {
		return nil, err
	}
	full, err := s.svcCtx.AccountRoleTrades.RoleSlotFull(ctx, in.GetAccount())
	switch {
	case err == nil:
		return &dspb.CheckAccountRoleSlotResponse{Full: full}, nil
	case errors.Is(err, accountdirectory.ErrInvalid):
		return nil, directoryError(err)
	default:
		logx.Errorf("[account-directory] check role slot failed: %v", err)
		return &dspb.CheckAccountRoleSlotResponse{Retry: true}, nil
	}
}
