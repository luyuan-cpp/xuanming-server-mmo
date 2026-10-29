package server

import (
	"context"
	"data_service/internal/metrics"
	"errors"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"time"
)

func (s *DataServiceServer) admitDirectory(ctx context.Context, method string, req proto.Message) error {
	if s.svcCtx.AccountDirectoryAuth == nil || s.svcCtx.AccountDirectory == nil {
		return status.Error(codes.Unavailable, "全局账号目录未就绪")
	}
	caller, err := s.svcCtx.AccountDirectoryAuth.Verify(ctx, accountdirectory.MethodPrefix+method, req)
	if err != nil || caller != "login" {
		return status.Error(codes.PermissionDenied, "账号目录调用证明无效")
	}
	return nil
}

// directoryError 把目录 store 的错误映射成 gRPC status;directoryRPCResult 按同一张映射给指标归类,两处一起改。
func directoryError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, accountdirectory.ErrInvalid):
		return status.Error(codes.InvalidArgument, "账号目录参数非法")
	case errors.Is(err, accountdirectory.ErrMissing):
		return status.Error(codes.NotFound, "账号角色目录不存在")
	case errors.Is(err, accountdirectory.ErrFull):
		return status.Error(codes.ResourceExhausted, "账号角色数量已达全局上限")
	case errors.Is(err, accountdirectory.ErrConflict):
		return status.Error(codes.FailedPrecondition, "账号角色归属或身份已变化")
	default:
		logx.Errorf("[account-directory] SQL依赖失败: %v", err)
		return status.Error(codes.Unavailable, "全局账号目录暂不可用")
	}
}

// observeDirectoryRPC 在每个账号目录 RPC 的 defer 里记 data_service_account_directory_rpc_seconds;start 在 defer 语句处求值,
// 即 handler 入口,所以鉴权失败等提前返回也计入。
func observeDirectoryRPC(rpc string, start time.Time, err *error) {
	metrics.ObserveAccountDirectoryRPC(rpc, directoryRPCResult(*err), time.Since(start))
}

// directoryRPCResult 按 handler 实际回给调用方的 gRPC code 归类,与 admitDirectory / admitRoleTrade / directoryError 的映射一一对应:
// NotFound(ErrMissing)→ not_found;PermissionDenied(调用证明无效)、InvalidArgument(ErrInvalid)、ResourceExhausted(ErrFull)、
// FailedPrecondition(ErrConflict)是明确拒绝 → rejected;Unavailable(目录未就绪、SQL 依赖失败)与其余一切
// (调用方取消 / 超时等)→ error。
func directoryRPCResult(err error) string {
	switch status.Code(err) {
	case codes.OK:
		return metrics.AccountDirectoryResultOK
	case codes.NotFound:
		return metrics.AccountDirectoryResultNotFound
	case codes.PermissionDenied, codes.InvalidArgument, codes.ResourceExhausted, codes.FailedPrecondition:
		return metrics.AccountDirectoryResultRejected
	default:
		return metrics.AccountDirectoryResultError
	}
}
func (s *DataServiceServer) ReadAccountDirectory(ctx context.Context, in *dspb.ReadAccountDirectoryRequest) (_ *dspb.AccountDirectoryResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCRead, time.Now(), &err)
	if err := s.admitDirectory(ctx, "ReadAccountDirectory", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountDirectory.Read(ctx, in.Account, in.CreateIfMissing)
	if err != nil {
		return nil, directoryError(err)
	}
	return &dspb.AccountDirectoryResponse{Players: out}, nil
}
func (s *DataServiceServer) CreateAccountRole(ctx context.Context, in *dspb.CreateAccountRoleRequest) (_ *dspb.AccountDirectoryResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCCreate, time.Now(), &err)
	if err := s.admitDirectory(ctx, "CreateAccountRole", in); err != nil {
		return nil, err
	}
	out, err := accountdirectory.CreateWithInitialCommit(ctx, s.svcCtx.AccountDirectory, in.Account, in.Role)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}
func (s *DataServiceServer) LookupAccountRole(ctx context.Context, in *dspb.LookupAccountRoleRequest) (_ *dspb.AccountRoleOwnership, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCLookup, time.Now(), &err)
	if s.svcCtx.AccountDirectory == nil {
		return nil, status.Error(codes.Unavailable, "全局账号目录未就绪")
	}
	if err := s.admitDirectory(ctx, "LookupAccountRole", in); err != nil {
		// 仅此只读方法允许trade独立密钥。Create/Repair/Remove仍只认login，不扩大写权限。
		accepted := false
		if auth := s.svcCtx.SceneManagerDirectoryAuth; auth != nil {
			caller, verifyErr := auth.Verify(ctx, accountdirectory.MethodPrefix+"LookupAccountRole", in)
			accepted = verifyErr == nil && caller == "scene_manager"
		}
		if !accepted {
			if s.svcCtx.AccountRoleTradeAuth == nil || s.svcCtx.AccountRoleTrades == nil {
				return nil, err
			}
			if tradeErr := s.admitRoleTrade(ctx, "LookupAccountRole", in); tradeErr != nil {
				return nil, tradeErr
			}
		}
	}
	out, err := s.svcCtx.AccountDirectory.Lookup(ctx, in.PlayerId)
	return out, directoryError(err)
}
func (s *DataServiceServer) RepairAccountRole(ctx context.Context, in *dspb.RepairAccountRoleRequest) (_ *dspb.AccountDirectoryResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCRepair, time.Now(), &err)
	if err := s.admitDirectory(ctx, "RepairAccountRole", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountDirectory.Repair(ctx, in.Account, in.Role, in.ExpectedOwnerRevision)
	if err != nil {
		return nil, directoryError(err)
	}
	return &dspb.AccountDirectoryResponse{Players: out}, nil
}
func (s *DataServiceServer) RemoveAccountRoles(ctx context.Context, in *dspb.RemoveAccountRolesRequest) (_ *emptypb.Empty, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCRemove, time.Now(), &err)
	if err := s.admitDirectory(ctx, "RemoveAccountRoles", in); err != nil {
		return nil, err
	}
	if err := s.svcCtx.AccountDirectory.Remove(ctx, in.Account, in.Roles); err != nil {
		return nil, directoryError(err)
	}
	return &emptypb.Empty{}, nil
}
