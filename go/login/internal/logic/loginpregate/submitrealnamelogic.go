package loginpregatelogic

import (
	"context"
	"errors"
	"time"

	"login/internal/logic/pkg/token"
	"login/internal/svc"
	base "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

// SubmitRealNameLogic 是 /api/real-name(docs/design/real-name-anti-addiction.md §5.2)。凭 /api/login 签发的 access token 鉴权,
// 账号与渠道只从令牌解出。姓名和身份证号只交给 realname.Service:本文件不记、不存,请求体也不进 RPC 统计日志
// (rpclogging.ProtectCredentials)。
type SubmitRealNameLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitRealNameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitRealNameLogic {
	return &SubmitRealNameLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SubmitRealName 结论都带内回:令牌不能用回 kLoginAccountNotFound(客户端回到登录),令牌存储故障回 kLoginAuthProviderUnavailable,
// 其余见 realname.Service.Submit。账号已经实名、或本渠道由平台实名时不提交,直接回当前状态。部署没开防沉迷时回空应答。
func (l *SubmitRealNameLogic) SubmitRealName(in *loginpb.SubmitRealNameRequest) (*loginpb.SubmitRealNameResponse, error) {
	resp := &loginpb.SubmitRealNameResponse{}
	aa := l.svcCtx.AntiAddiction
	if aa == nil {
		return resp, nil
	}
	data, err := l.svcCtx.TokenManager.ValidateAccess(l.ctx, in.GetAccessToken())
	if err != nil {
		if errors.Is(err, token.ErrAccessRejected) {
			resp.ErrorMessage = &base.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
			return resp, nil
		}
		l.Errorf("[real-name] access token lookup failed: %v", err)
		resp.ErrorMessage = &base.TipInfoMessage{Id: uint32(table.LoginError_kLoginAuthProviderUnavailable)}
		return resp, nil
	}
	account, channel := data.Account, data.AuthType

	realNames, ok := l.svcCtx.RealNames()
	if !ok {
		resp.ErrorMessage = &base.TipInfoMessage{Id: uint32(table.LoginError_kLoginRealNameServiceUnavailable)}
		return resp, nil
	}
	directory, err := realNames.ReadWithLifecycle(l.ctx, account, false)
	pi := directory.GetRealNamePi()
	if err != nil {
		l.Errorf("[real-name] read account directory failed: account=%s err=%v", account, err)
		resp.ErrorMessage = &base.TipInfoMessage{Id: uint32(table.LoginError_kLoginRealNameServiceUnavailable)}
		return resp, nil
	}
	if pi == "" && aa.SelfManaged(channel) {
		var tip uint32
		pi, tip = aa.RealName.Submit(l.ctx, account, in.GetName(), in.GetIdNumber())
		if tip != 0 {
			resp.ErrorMessage = &base.TipInfoMessage{Id: tip}
		}
	}
	resp.AntiAddiction = aa.Evaluate(channel, pi, time.Now()).Status
	return resp, nil
}
