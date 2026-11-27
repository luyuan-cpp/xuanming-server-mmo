package clientplayerloginlogic

import (
	"context"
	"errors"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// passportSessionCheckTimeout 是刷新前问一次通行证的时限。管理接口在集群内网,正常十几毫秒就回;超过它按核对不了处理,
// 不把续期本身拖到 RPC 超时(login 缺省 4s)。
const passportSessionCheckTimeout = time.Second

var passportSessionCheckTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "passport",
	Name:      "session_check_total",
	Help: "Official passport refreshes checked against the passport before renewal, by result: valid (renewed), " +
		"user_gone / user_forbidden / password_changed (all tokens of the account revoked, refresh refused), " +
		"revoke_failed (refresh refused, checked again next time) and error (passport unreachable, renewed anyway). " +
		"A sustained error rate means password changes and bans in the passport no longer log players out.",
	Labels: []string{"result"},
})

type RefreshTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRefreshTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RefreshTokenLogic {
	return &RefreshTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RefreshTokenLogic) RefreshToken(in *login_proto.RefreshTokenRequest) (*login_proto.RefreshTokenResponse, error) {
	resp := &login_proto.RefreshTokenResponse{}

	if in.RefreshToken == "" {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
		return resp, nil
	}

	// 与令牌失效同一个码:客户端照常走完整重登。
	if l.passportSessionRevoked(in.RefreshToken) {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
		return resp, nil
	}

	// 宽限窗口内对同一 refresh token 的重放返回同一对新令牌(见 token.Manager.Refresh),
	// 所以回包丢失后客户端拿原 token 重试是安全的;窗口外重放已轮换的旧 token 会吊销它的家族。
	// 错误里不含令牌值。
	tokenPair, err := l.svcCtx.TokenManager.Refresh(l.ctx, in.RefreshToken)
	if err != nil {
		if errors.Is(err, token.ErrRefreshRejected) {
			logx.Infof("RefreshToken rejected: %v", err)
		} else {
			logx.Errorf("RefreshToken failed: %v", err)
		}
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
		return resp, nil
	}

	resp.AccessToken = tokenPair.AccessToken
	resp.RefreshToken = tokenPair.RefreshToken
	resp.AccessTokenExpire = tokenPair.AccessTokenExpire
	resp.RefreshTokenExpire = tokenPair.RefreshTokenExpire

	logx.Infof("RefreshToken success: new access_token expires=%d", tokenPair.AccessTokenExpire)
	return resp, nil
}

// passportSessionRevoked 在续期之前核对官方通行证账号(docs/design/official-passport-registration.md §3.6):这次登录之后
// 玩家在通行证里改过密码(含找回密码)、被封禁或被删除,就吊销这个账号的全部令牌,每台设备都要重新登录;回 true 表示不续期。
//
// 放在轮换之前:吊销没成功就不续期,旧令牌还在,下一次刷新再核对、再吊销。放在之后的话,新的一对已经写下,
// 客户端重试走宽限窗口的重放会原样拿到它,绕过核对。
//
// 核对不了(通行证或网络故障)照常续期、计 error:通行证出故障不能把在线玩家都踢下线。判断依据是载荷里的身份令牌签发时刻,
// 轮换时原样带下去,故障期间续出来的令牌在下一次核对时照样判得出来。
// 已经连在 gate 上的会话不踢,与吊销的既有口径一致(dual-token-authentication.md「已进游戏的连接」),断线后重连时才失效。
func (l *RefreshTokenLogic) passportSessionRevoked(refreshToken string) bool {
	admin := l.svcCtx.PassportAdmin
	if admin == nil {
		return false
	}
	data, err := l.svcCtx.TokenManager.ReadRefreshPayload(l.ctx, refreshToken)
	if err != nil || data == nil || data.AuthType != auth.PassportAuthType {
		return false // 令牌不在、读不出来或不是通行证登录:交给 Refresh 照原规则判
	}
	sub, ok := auth.PassportSubject(data.Account)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(l.ctx, passportSessionCheckTimeout)
	defer cancel()
	reason, err := admin.CheckSession(ctx, sub, data.IdentityIssuedAt)
	if err != nil {
		passportSessionCheckTotal.Inc("error")
		l.Errorf("[passport] session check failed, renewing anyway: account=%s: %v", data.Account, err)
		return false
	}
	if reason == "" {
		passportSessionCheckTotal.Inc("valid")
		return false
	}
	if err := l.svcCtx.TokenManager.RevokeAll(l.ctx, data.Account); err != nil {
		passportSessionCheckTotal.Inc("revoke_failed")
		l.Errorf("[passport] revoking sessions failed, refresh refused: account=%s reason=%s: %v", data.Account, reason, err)
		return true
	}
	passportSessionCheckTotal.Inc(reason)
	l.Infof("[passport] all sessions of the account revoked: account=%s reason=%s", data.Account, reason)
	return true
}
