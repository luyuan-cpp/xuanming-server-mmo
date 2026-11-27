package clientplayerloginlogic

import (
	"context"
	"errors"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_proto "proto/login"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

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

	// 官方通行证账号:轮换之前核对通行证里有没有改密、被封、被删(pkg/passportsession)。不续期时回与令牌失效同一个码,
	// 客户端照常走完整重登。令牌不在或读不出来时交给 Refresh 照原规则判。
	if l.passportSessionRefused(in.RefreshToken) {
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

// passportSessionRefused 读出 refresh token 的载荷交给 PassportSessions.CheckRefresh;没配通行证管理凭据时直接放行。
func (l *RefreshTokenLogic) passportSessionRefused(refreshToken string) bool {
	if l.svcCtx.PassportSessions == nil {
		return false
	}
	data, err := l.svcCtx.TokenManager.ReadRefreshPayload(l.ctx, refreshToken)
	if err != nil || data == nil {
		return false
	}
	return l.svcCtx.PassportSessions.CheckRefresh(l.ctx, data)
}
