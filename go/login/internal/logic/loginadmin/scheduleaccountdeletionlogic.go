package loginadminlogic

import (
	"context"
	"errors"
	"unicode/utf8"

	"login/internal/logic/pkg/accountdeletion"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	"proto/login"
	"shared/accountdirectory"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ScheduleAccountDeletionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewScheduleAccountDeletionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ScheduleAccountDeletionLogic {
	return &ScheduleAccountDeletionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ScheduleAccountDeletion 客服代办注销(Google Play 要求的网页入口,docs/design/account-deletion.md §4.6)。
// 鉴权与双人复核在拦截器(x-admin-token、admin_approval.go)。
// 按角色找到账号,走与本人申请同一套预检与冷静期,data_service 的回执记 source=2。客服先在系统外核实身份。
// 这条路拿不到 Apple 授权码,Apple 账号不撤销授权,由客服告诉玩家到 Apple 设置里手动移除(TN3194 的兜底)。
func (l *ScheduleAccountDeletionLogic) ScheduleAccountDeletion(in *login.ScheduleAccountDeletionRequest) (*login.AccountDeletionResponse, error) {
	if in.GetPlayerId() == 0 || !boundedText(in.GetOperator(), 64) || !boundedText(in.GetReason(), 256) {
		return nil, status.Error(codes.InvalidArgument, "player_id, operator (<= 64 bytes) and reason (<= 256 bytes) are required")
	}
	if l.svcCtx.AccountDeletion == nil || l.svcCtx.AccountDirectory == nil {
		return nil, status.Error(codes.Unavailable, "account deletion is not ready")
	}
	owned, err := l.svcCtx.AccountDirectory.Lookup(l.ctx, in.GetPlayerId())
	if errors.Is(err, accountdirectory.ErrMissing) {
		return &login.AccountDeletionResponse{ErrorMessage: &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}}, nil
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "account directory lookup failed")
	}
	l.Infof("[account-deletion] admin schedule: player=%d account=%s operator=%s approval_id=%q approver=%q reason=%q peer=%s",
		in.GetPlayerId(), accountdeletion.MaskAccount(owned.GetAccount()), in.GetOperator(), in.GetApproval().GetApprovalId(),
		in.GetApproval().GetApprover(), in.GetReason(), peerAddrOf(l.ctx))
	return l.svcCtx.AccountDeletion.Request(l.ctx, owned.GetAccount(), accountdeletion.SourceAdmin, false, ""), nil
}

func boundedText(s string, maxBytes int) bool {
	return s != "" && len(s) <= maxBytes && utf8.ValidString(s)
}
