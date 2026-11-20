package clientplayerloginlogic

import (
	"context"

	"login/internal/svc"
	login_proto "proto/login"
)

// ResolveLoginAccount 是登录认证凭据的那一步(LoginLogic.resolveAccount),给要求重新认证的入口复用(申请注销,
// docs/design/account-deletion.md §4.2):口令配额、渠道校验、身份令牌一次性消费都与登录同一套,不另写一份。
func ResolveLoginAccount(ctx context.Context, svcCtx *svc.ServiceContext, in *login_proto.LoginRequest) (string, error) {
	return NewLoginLogic(ctx, svcCtx).resolveAccount(in)
}

// AuthFailureTip 同 authFailureTip:渠道或配置故障回故障码,其余(凭证无效等)回 kLoginAccountNotFound。
func AuthFailureTip(err error) uint32 {
	return authFailureTip(err)
}
