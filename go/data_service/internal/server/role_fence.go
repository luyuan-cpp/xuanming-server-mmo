package server

import (
	"context"
	"errors"

	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 这两个方法都是只读,没有结局未知。结论和读失败都带内回:读失败回 Unavailable 的话,PublicationAdmission 会把排空证明打成 Hold。
// gRPC 错误只留给没装配、鉴权失败和调用方缺陷。

// GetRoleFenceState 回角色护栏的权威状态(docs/design/horizontal-scale-logical-db.md §3.6),强读。
// 调用方是参与者和 chat,各用自己的密钥;只读,所以不再按方法区分调用方。目录里没有这个角色回 found=false,读不到回 retry=true。
func (s *DataServiceServer) GetRoleFenceState(ctx context.Context, in *dspb.GetRoleFenceStateRequest) (*dspb.GetRoleFenceStateResponse, error) {
	if s.svcCtx.RoleFenceStates == nil || s.svcCtx.RoleFenceAuth == nil {
		return nil, status.Error(codes.Unavailable, "角色护栏目录未就绪")
	}
	if _, err := s.svcCtx.RoleFenceAuth.Verify(ctx, accountdirectory.MethodPrefix+"GetRoleFenceState", in); err != nil {
		return nil, status.Error(codes.PermissionDenied, "角色护栏调用证明无效")
	}
	state, err := s.svcCtx.RoleFenceStates.RoleFenceState(ctx, in.GetPlayerId())
	switch {
	case err == nil:
		return &dspb.GetRoleFenceStateResponse{Found: true, State: state}, nil
	case errors.Is(err, accountdirectory.ErrMissing):
		return &dspb.GetRoleFenceStateResponse{}, nil
	case errors.Is(err, accountdirectory.ErrInvalid):
		return nil, directoryError(err)
	default:
		logx.Errorf("[role-fence] read state failed: player=%d err=%v", in.GetPlayerId(), err)
		return &dspb.GetRoleFenceStateResponse{Retry: true}, nil
	}
}

// VerifyRoleAdmission 回一份入场登记是否仍然有效(规格 §3.7),强读。调用方与 GetRoleFenceState 相同。
// 原件不合法回 InvalidArgument:调用方自己先核过,走到这里是缺陷,不能当成「登记已失效」。读不到回 retry=true。
func (s *DataServiceServer) VerifyRoleAdmission(ctx context.Context, in *dspb.VerifyRoleAdmissionRequest) (*dspb.VerifyRoleAdmissionResponse, error) {
	if s.svcCtx.RoleAdmissionChecks == nil || s.svcCtx.RoleFenceAuth == nil {
		return nil, status.Error(codes.Unavailable, "角色入场校验未就绪")
	}
	if _, err := s.svcCtx.RoleFenceAuth.Verify(ctx, accountdirectory.MethodPrefix+"VerifyRoleAdmission", in); err != nil {
		return nil, status.Error(codes.PermissionDenied, "角色护栏调用证明无效")
	}
	valid, err := s.svcCtx.RoleAdmissionChecks.VerifyRoleAdmission(ctx, in.GetAdmission())
	switch {
	case err == nil:
		return &dspb.VerifyRoleAdmissionResponse{Valid: valid}, nil
	case errors.Is(err, accountdirectory.ErrInvalid):
		return nil, directoryError(err)
	default:
		logx.Errorf("[role-fence] verify admission failed: player=%d err=%v", in.GetAdmission().GetPlayerId(), err)
		return &dspb.VerifyRoleAdmissionResponse{Retry: true}, nil
	}
}
