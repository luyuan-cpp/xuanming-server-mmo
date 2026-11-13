package loginpregatelogic

import (
	"context"
	"errors"

	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/token"
	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// AssignGate 的鉴权:这次分配以谁的身份进行。
//
// 身份只从 /api/login 签发的 access token 解出,不看请求正文。2026-11-12 之前请求里带的是客户端自报的 account:
// login 拿它判「这个账号已有在线会话、免排队」并当排队身份,报出别人的账号名就能插队;第三方登录的客户端又不知道
// 自己的账号名(wx_<unionid> 由服务端算出),合法的免排队反而用不上。
//
// 没带令牌的匿名分配只为旧流程保留:旧 gate 凭据登录是「先分配 gate、连上之后才认证」,分配时还没有令牌
// (robot 也是这个顺序)。所以匿名分配与旧路径同一个开关(LegacyGateLoginEnabled):运维关掉旧路径时匿名分配一起关,
// 不留一个不认证就能占排队位的入口。关之前看 login_assign_gate_auth_total{result="anonymous"} 降到零。

// assignGateAuthTotal 按鉴权结果统计 AssignGate(不含带 queue_token 的排队重入,排队令牌本身就是先前已分配的凭证):
//   - authenticated:带了有效的 access token;
//   - anonymous:没带令牌,旧路径开着,按匿名分配(不免排队,排队身份为空);
//   - rejected_anonymous:没带令牌,旧路径已关;
//   - rejected_token:令牌不能用(格式、未知、过期、家族已吊销);
//   - error:令牌存储故障,回 StatusError 让客户端稍后重试。
var assignGateAuthTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "assign_gate",
	Name:      "auth_total",
	Help: "AssignGate calls (queue re-entry excluded) by authentication result: " +
		"authenticated, anonymous, rejected_anonymous, rejected_token, error.",
	Labels: []string{"result"},
})

// accessTokenValidator 是鉴权需要的最小接口,*token.Manager 实现它;单测换成假实现。
type accessTokenValidator interface {
	ValidateAccess(ctx context.Context, accessToken string) (*token.TokenData, error)
}

// authenticateAssignGate 返回 (account, nil) 表示放行:account 非空 = 已鉴权,空串 = 匿名(只在旧路径开着时)。
// 返回 (_, resp) 表示拒绝或故障,调用方原样回给网关。日志不带令牌原文(ValidateAccess 的错误本身不含令牌值)。
func authenticateAssignGate(ctx context.Context, validator accessTokenValidator, accessToken string,
	legacyGateLoginEnabled bool) (string, *loginpb.AssignGateResponse) {
	if accessToken == "" {
		if legacyGateLoginEnabled {
			assignGateAuthTotal.Inc("anonymous")
			return "", nil
		}
		assignGateAuthTotal.Inc("rejected_anonymous")
		return "", unauthenticatedAssignGate("access token required")
	}

	data, err := validator.ValidateAccess(ctx, accessToken)
	if err != nil {
		if errors.Is(err, token.ErrAccessRejected) {
			assignGateAuthTotal.Inc("rejected_token")
			logx.WithContext(ctx).Infof("[assign-gate] access token rejected: %v", err)
			return "", unauthenticatedAssignGate("access token invalid")
		}
		assignGateAuthTotal.Inc("error")
		logx.WithContext(ctx).Errorf("[assign-gate] access token lookup failed: %v", err)
		return "", &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusError), Error: "token lookup failed"}
	}
	assignGateAuthTotal.Inc("authenticated")
	return data.Account, nil
}

func unauthenticatedAssignGate(reason string) *loginpb.AssignGateResponse {
	return &loginpb.AssignGateResponse{Status: uint32(loginqueue.StatusUnauthenticated), Error: reason}
}
