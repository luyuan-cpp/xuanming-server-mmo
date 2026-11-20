package server

import (
	"context"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/store"
	dspb "proto/data_service"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 账号注销的四个 RPC(docs/design/account-deletion.md §4、§5.1)。鉴权同账号目录:只认 login 的调用证明。
// 结论带内回(AccountDeletionOutcome),gRPC 错误只留给鉴权、非法参数和结局未知的写:四个动作都幂等,login 同参数重放即可。

func (s *DataServiceServer) admitDeletion(ctx context.Context, method string, req proto.Message) error {
	if err := s.admitDirectory(ctx, method, req); err != nil {
		return err
	}
	if s.svcCtx.AccountDeletions == nil {
		return status.Error(codes.Unavailable, "account deletion is not configured")
	}
	return nil
}

func (s *DataServiceServer) RequestAccountDeletion(ctx context.Context, in *dspb.RequestAccountDeletionRequest) (_ *dspb.AccountDeletionResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCDeletionRequest, time.Now(), &err)
	if err := s.admitDeletion(ctx, "RequestAccountDeletion", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountDeletions.RequestDeletion(ctx, in.Account, in.Source, s.svcCtx.DeletionCoolingOff)
	if err != nil {
		return nil, directoryError(err)
	}
	if out.Outcome == dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK {
		logx.Infof("[account-deletion] requested: source=%d state=%s execute_after_ms=%d", in.Source, out.Lifecycle.GetDeletionState(), out.Lifecycle.GetDeletionExecuteAfterMs())
	}
	return out, nil
}

func (s *DataServiceServer) CancelAccountDeletion(ctx context.Context, in *dspb.CancelAccountDeletionRequest) (_ *dspb.AccountDeletionResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCDeletionCancel, time.Now(), &err)
	if err := s.admitDeletion(ctx, "CancelAccountDeletion", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountDeletions.CancelDeletion(ctx, in.Account)
	if err != nil {
		return nil, directoryError(err)
	}
	return out, nil
}

func (s *DataServiceServer) ClaimAccountDeletions(ctx context.Context, in *dspb.ClaimAccountDeletionsRequest) (_ *dspb.ClaimAccountDeletionsResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCDeletionClaim, time.Now(), &err)
	if err := s.admitDeletion(ctx, "ClaimAccountDeletions", in); err != nil {
		return nil, err
	}
	if in.Limit < 1 || in.Limit > store.MaxDeletionClaim {
		return nil, status.Errorf(codes.InvalidArgument, "limit must be within [1, %d]", store.MaxDeletionClaim)
	}
	accounts, err := s.svcCtx.AccountDeletions.ClaimDeletions(ctx, int(in.Limit))
	if err != nil {
		// 认领到的照样交出去:没认领成的那几行等租约过期再被认领,不会丢。
		logx.Errorf("[account-deletion] claim partially failed: claimed=%d err=%v", len(accounts), err)
		if len(accounts) == 0 {
			return nil, directoryError(err)
		}
	}
	return &dspb.ClaimAccountDeletionsResponse{Accounts: accounts}, nil
}

func (s *DataServiceServer) FinishAccountDeletion(ctx context.Context, in *dspb.FinishAccountDeletionRequest) (_ *dspb.FinishAccountDeletionResponse, err error) {
	defer observeDirectoryRPC(metrics.AccountDirectoryRPCDeletionFinish, time.Now(), &err)
	if err := s.admitDeletion(ctx, "FinishAccountDeletion", in); err != nil {
		return nil, err
	}
	out, err := s.svcCtx.AccountDeletions.FinishDeletion(ctx, in.Account)
	if err != nil {
		return nil, directoryError(err)
	}
	if out.Done {
		logx.Infof("[account-deletion] account record deleted, receipt written")
	}
	return out, nil
}
