package roleidentity

import (
	"context"
	"errors"
	"fmt"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/callerauth"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrUnverifiedSession 表示在线请求没有可用的 gate 签名原身份。调用方按各自入口语义拒绝,
// 不得退回读登录 Redis,也不得按 player_id 反查当前归属替旧请求补权。
var ErrUnverifiedSession = errors.New("在线请求缺少 gate 签名的原角色身份")

// 入口拒绝原因,作 label 用(低基数,不放方法名与玩家标识)。入口准入用下面四个;归属预检另有两个,见 ownership.go。
const (
	rejectClientWithoutSession  = "client_without_session"
	rejectInternalWithSession   = "internal_with_session"
	rejectUnverifiedSession     = "unverified_session"
	rejectIncompleteOnlineActor = "incomplete_identity"
)

var entryRejectedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "online_entry",
	Name:      "rejected_total",
	Help:      "在线域入口拒绝次数;入口准入的 reason 取 client_without_session / internal_with_session / unverified_session / incomplete_identity,归属预检取 ownership_changed / ownership_unverified。",
	Labels:    []string{"reason"},
})

type sessionKey struct{}

// Session 返回入口准入后的客户端会话副本。生产中只有 EntryInterceptor 会放:它取自 gate 签名的验签原件,
// 且只给带完整原身份的客户端请求放;内部调用与登记的匿名只读例外都没有。
func Session(ctx context.Context) (*base.SessionDetails, bool) {
	detail, ok := ctx.Value(sessionKey{}).(*base.SessionDetails)
	if !ok || detail == nil {
		return nil, false
	}
	return proto.Clone(detail).(*base.SessionDetails), true
}

// WithSession 把客户端会话放进 ctx。生产只由 EntryInterceptor 调用,放的是验签原件;
// 业务逻辑单测用它造「已过入口」的客户端来源。不得用它包装任何没经 callerauth 验签的声明。
// detail 为 nil 时 Session 返回 false。
func WithSession(ctx context.Context, detail *base.SessionDetails) context.Context {
	if detail == nil {
		return context.WithValue(ctx, sessionKey{}, (*base.SessionDetails)(nil))
	}
	return context.WithValue(ctx, sessionKey{}, proto.Clone(detail).(*base.SessionDetails))
}

// SessionOwnership 返回在线请求的原角色归属:原认证账号、原归属修订与角色。
//
// 在线域(match / chat / friend / guild / trade / mail / rank)的原账号只有一个来源:login 在 EnterGame 时
// 捕获、经 BindSessionEvent 交给 gate、由 gate 写进 SessionDetails 并对整份字节签名的 authenticated_account。
// 这里只读 callerauth 验签拦截器存进 ctx 的原件,要求它带角色、原归属修订与合法账号;缺任何一项都拒绝:
// gate 只在角色绑定后才写账号,没有账号的请求不属于在线角色(未选角、已离场或换了连接)。
//
// 这一步只证明「这是哪条连接、哪个账号、哪次归属」;角色此刻是否仍归该账号所有、是否冻结或删除,
// 由业务在提交点锁本库 role_fence 复核(docs/design/horizontal-scale-logical-db.md §3)。
func SessionOwnership(ctx context.Context) (*dspb.AccountRoleOwnership, error) {
	verified, ok := callerauth.AuthenticatedSession(ctx)
	if !ok || verified.GetPlayerId() == 0 || verified.GetOwnerRevision() == 0 ||
		!accountdirectory.ValidAccount(verified.GetAuthenticatedAccount()) {
		return nil, ErrUnverifiedSession
	}
	return &dspb.AccountRoleOwnership{
		Account:       verified.GetAuthenticatedAccount(),
		OwnerRevision: verified.GetOwnerRevision(),
		Role:          &base.AccountSimplePlayer{PlayerId: verified.GetPlayerId()},
	}, nil
}

// CaptureSession 把验签原件里的原角色归属捕获进 ctx(之后 Original 可取),供提交点按原件复核。
// 失败时返回的 ctx 为 nil,调用方按各自入口语义拒绝。
func CaptureSession(ctx context.Context) (context.Context, error) {
	owned, err := SessionOwnership(ctx)
	if err != nil {
		return nil, err
	}
	return Capture(ctx, owned)
}

// EntryInterceptor 是在线域服务唯一的客户端入口准入(docs/design/gate-online-identity.md §9.2)。
// 装在 callerauth.GateInterceptor 之后、业务之前,即各服务原先会话解码拦截器的位置。
//
//   - 客户端方法(callable 为真)带会话:验签原件须带角色、原修订与合法账号,原件放进 ctx(Session 可取)、
//     原角色归属捕获进 ctx(Original 可取)后放行;缺任一项 PermissionDenied。
//   - 客户端方法不带会话:PermissionDenied。只有 anonymousReads 里登记的只读方法例外,按内部调用放行,ctx 里没有会话。
//   - 非客户端方法带会话:PermissionDenied,客户端不得调内部方法;不带会话:放行,由方法自己的调用方准入负责(serviceauth 等)。
//   - 带会话头却没有验签原件:Unauthenticated。只会是装配错误(GateInterceptor 没装在前面),fail-closed。
//
// callable 由装配层注入(生产传 clientrpc.Callable),本包不依赖生成物。anonymousReads 必须都是客户端方法,
// 否则返回错误,调用方拒启。
func EntryInterceptor(callable func(fullMethod string) bool, anonymousReads ...string) (grpc.UnaryServerInterceptor, error) {
	if callable == nil {
		return nil, errors.New("在线入口准入缺客户端方法判定")
	}
	anonymous := make(map[string]struct{}, len(anonymousReads))
	for _, method := range anonymousReads {
		if !callable(method) {
			return nil, fmt.Errorf("在线入口准入的匿名只读例外 %s 不是客户端方法", method)
		}
		anonymous[method] = struct{}{}
	}
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		client := callable(info.FullMethod)
		if len(md.Get(callerauth.MetaSessionDetail)) == 0 {
			if !client {
				return next(ctx, request)
			}
			if _, ok := anonymous[info.FullMethod]; ok {
				return next(ctx, request)
			}
			// 只有内部进程会不带会话调客户端方法:必然是调用方写错了,不是玩家行为。
			entryRejectedTotal.Inc(rejectClientWithoutSession)
			logx.WithContext(ctx).Errorf("[online-entry] Rejecting %s without gate session: client methods accept only requests forwarded by gate", info.FullMethod)
			return nil, status.Errorf(codes.PermissionDenied, "%s accepts only client requests forwarded by gate", info.FullMethod)
		}
		verified, ok := callerauth.AuthenticatedSession(ctx)
		if !ok {
			entryRejectedTotal.Inc(rejectUnverifiedSession)
			logx.WithContext(ctx).Errorf("[online-entry] Rejecting %s: session metadata present but not verified (GateInterceptor must run first)", info.FullMethod)
			return nil, status.Error(codes.Unauthenticated, "session is not verified")
		}
		if !client {
			entryRejectedTotal.Inc(rejectInternalWithSession)
			logx.WithContext(ctx).Errorf("[online-entry] Rejecting client call to non-client method %s (session_id=%d)", info.FullMethod, verified.GetSessionId())
			return nil, status.Errorf(codes.PermissionDenied, "%s is not callable by clients", info.FullMethod)
		}
		bound, err := CaptureSession(ctx)
		if err != nil {
			// 客户端驱动(离场后仍发在线请求、绑定未到时抢发),按请求计数,不逐条打日志。
			entryRejectedTotal.Inc(rejectIncompleteOnlineActor)
			return nil, status.Error(codes.PermissionDenied, "online request has no original role identity")
		}
		return next(WithSession(bound, verified), request)
	}, nil
}
