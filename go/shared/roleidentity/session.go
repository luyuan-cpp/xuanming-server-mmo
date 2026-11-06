package roleidentity

import (
	"context"
	"errors"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/callerauth"

	"google.golang.org/protobuf/proto"
)

// ErrUnverifiedSession 表示在线请求没有可用的 gate 签名原身份。调用方按各自入口语义拒绝,
// 不得退回读登录 Redis,也不得按 player_id 反查当前归属替旧请求补权。
var ErrUnverifiedSession = errors.New("在线请求缺少 gate 签名的原角色身份")

// SessionAccount 返回在线请求的原认证账号。
//
// 在线域(match / chat / friend / guild / trade / mail)的原账号只有一个来源:login 在 EnterGame 时
// 捕获、经 BindSessionEvent 交给 gate、由 gate 写进 SessionDetails 并对整份字节签名的 authenticated_account。
// callerauth 的验签拦截器验过签名后把原件存进 ctx;这里要求业务解码出的 claimed 与原件逐字段相同,
// 且带角色、原归属修订与合法账号。缺任何一项都拒绝:gate 只在角色绑定后才写账号,没有账号的请求
// 不属于在线角色(未选角、已离场或换了连接)。
//
// 这一步只证明「这是哪条连接、哪个账号、哪次归属」;角色此刻是否仍归该账号所有、是否冻结或删除,
// 由业务在提交点锁本库 role_fence 复核(docs/design/horizontal-scale-logical-db.md §3)。
func SessionAccount(ctx context.Context, claimed *base.SessionDetails) (string, error) {
	verified, ok := callerauth.AuthenticatedSession(ctx)
	if !ok || claimed == nil || !proto.Equal(verified, claimed) ||
		verified.GetPlayerId() == 0 || verified.GetOwnerRevision() == 0 ||
		!accountdirectory.ValidAccount(verified.GetAuthenticatedAccount()) {
		return "", ErrUnverifiedSession
	}
	return verified.GetAuthenticatedAccount(), nil
}

// CaptureSession 把在线请求的原角色归属(签名原账号 + 原修订 + 角色)捕获进 ctx,供提交点按原件复核。
// 失败时返回的 ctx 为 nil,调用方按各自入口语义拒绝。
func CaptureSession(ctx context.Context, claimed *base.SessionDetails) (context.Context, error) {
	account, err := SessionAccount(ctx, claimed)
	if err != nil {
		return nil, err
	}
	return Capture(ctx, &dspb.AccountRoleOwnership{
		Account:       account,
		OwnerRevision: claimed.GetOwnerRevision(),
		Role:          &base.AccountSimplePlayer{PlayerId: claimed.GetPlayerId()},
	})
}
