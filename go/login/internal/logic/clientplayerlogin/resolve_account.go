package clientplayerloginlogic

import (
	"context"
	"errors"

	"login/internal/logic/pkg/auth"
	"login/internal/svc"
	login_proto "proto/login"
)

// errPassportSessionRefused:官方通行证核对不认这次身份令牌(改密之前签的、用户被封被删);authFailureTip 回 kLoginPassportCredentialChanged。
var errPassportSessionRefused = errors.New("passport session refused: password changed after the identity token, or the user is banned or deleted")

// ResolveLoginAccount 是登录认证凭据的那一步(LoginLogic.resolveAccount),给要求重新认证的入口复用(申请注销,
// docs/design/account-deletion.md §4.2):口令配额、渠道校验、身份令牌一次性消费都与登录同一套,不另写一份。
// 官方通行证账号也过登录时那道核对(docs/design/official-passport-registration.md §3.6):登录会被拒的身份令牌
// (改密之前签的、用户被封被删),重新认证同样拒,免得一枚截获的旧令牌拿去发起注销。
func ResolveLoginAccount(ctx context.Context, svcCtx *svc.ServiceContext, in *login_proto.LoginRequest) (string, error) {
	resolved, err := NewLoginLogic(ctx, svcCtx).resolveAccount(in)
	if err != nil {
		return "", err
	}
	if in.GetAuthType() == auth.PassportAuthType &&
		svcCtx.PassportSessions.CheckLogin(ctx, resolved.Account, resolved.IdentityIssuedAt) != 0 {
		return "", errPassportSessionRefused
	}
	return resolved.Account, nil
}

// AuthFailureTip 同 authFailureTip:渠道或配置故障回故障码,通行证核对不认回 kLoginPassportCredentialChanged,
// 其余(凭证无效等)回 kLoginAccountNotFound。
func AuthFailureTip(err error) uint32 {
	return authFailureTip(err)
}
