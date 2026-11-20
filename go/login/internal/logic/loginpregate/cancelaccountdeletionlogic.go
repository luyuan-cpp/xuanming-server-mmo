package loginpregatelogic

import (
	"context"
	"errors"

	"login/internal/logic/pkg/token"
	"login/internal/svc"
	"proto/login"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelAccountDeletionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelAccountDeletionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelAccountDeletionLogic {
	return &CancelAccountDeletionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CancelAccountDeletion 撤销注销(docs/design/account-deletion.md §4.4),网关 POST /api/account/delete/cancel 调。
// 凭冷静期内 /api/login 签发的 access token:撤销只把账号恢复原样,不需要再拉起渠道。令牌被拒回 2000,存储故障回故障码。
func (l *CancelAccountDeletionLogic) CancelAccountDeletion(in *login.CancelAccountDeletionRequest) (*login.AccountDeletionResponse, error) {
	if l.svcCtx.AccountDeletion == nil || l.svcCtx.TokenManager == nil {
		return deletionTip(table.LoginError_kLoginDataSerializeFailed), nil
	}
	if in.GetAccessToken() == "" {
		return deletionTip(table.LoginError_kLoginAccountNotFound), nil
	}
	data, err := l.svcCtx.TokenManager.ValidateAccess(l.ctx, in.GetAccessToken())
	if errors.Is(err, token.ErrAccessRejected) {
		return deletionTip(table.LoginError_kLoginAccountNotFound), nil
	}
	if err != nil {
		l.Errorf("[account-deletion] access token lookup failed on cancel: %v", err)
		return deletionTip(table.LoginError_kLoginRedisError), nil
	}
	return l.svcCtx.AccountDeletion.Cancel(l.ctx, data.Account), nil
}
