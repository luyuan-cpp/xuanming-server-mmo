package loginpregatelogic

import (
	"context"

	clientplayerloginlogic "login/internal/logic/clientplayerlogin"
	"login/internal/logic/pkg/accountdeletion"
	"login/internal/logic/pkg/auth"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	"proto/login"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

type RequestAccountDeletionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRequestAccountDeletionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RequestAccountDeletionLogic {
	return &RequestAccountDeletionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RequestAccountDeletion 申请注销(docs/design/account-deletion.md §4.2),网关 POST /api/account/delete 调。
// 要求重新认证:凭据与 /api/login 同义,用登录同一个认证步骤;不收 access token(那只证明「这台设备登录过」)。
// Apple 账号随申请带上这次重新授权拿到的授权码,用来撤销 Sign in with Apple 授权(§8)。
func (l *RequestAccountDeletionLogic) RequestAccountDeletion(in *login.AccountDeletionRequest) (*login.AccountDeletionResponse, error) {
	if l.svcCtx.AccountDeletion == nil {
		return deletionTip(table.LoginError_kLoginDataSerializeFailed), nil
	}
	authType := in.GetAuthType()
	if authType == "" {
		authType = "password"
	}
	if authType == "access_token" {
		return deletionTip(table.LoginError_kLoginAccountNotFound), nil
	}
	account, err := clientplayerloginlogic.ResolveLoginAccount(l.ctx, l.svcCtx, &login.LoginRequest{
		Account: in.GetAccount(), Password: in.GetPassword(), AuthType: in.GetAuthType(), AuthToken: in.GetAuthToken(),
	})
	if err != nil {
		tip := clientplayerloginlogic.AuthFailureTip(err)
		l.Infof("[account-deletion] re-authentication failed: type=%s tip=%d err=%v", authType, tip, err)
		return &login.AccountDeletionResponse{ErrorMessage: &login_proto_common.TipInfoMessage{Id: tip}}, nil
	}
	return l.svcCtx.AccountDeletion.Request(l.ctx, account, accountdeletion.SourceUser,
		authType == auth.AppleAuthType, in.GetAuthorizationCode()), nil
}

func deletionTip(id table.LoginError) *login.AccountDeletionResponse {
	return &login.AccountDeletionResponse{ErrorMessage: &login_proto_common.TipInfoMessage{Id: uint32(id)}}
}
