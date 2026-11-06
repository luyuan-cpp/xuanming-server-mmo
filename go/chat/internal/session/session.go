// Package session 存取入口准入放进 ctx 的客户端会话。
//
// 会话只有一个来源:roleidentity.EntryInterceptor 从 gate 签名的验签原件放进 ctx,且只给带完整原身份的
// 客户端请求放(docs/design/gate-online-identity.md §9.2)。本包只是 chat 业务代码沿用的取值名字,不自己解 metadata。
// player_id 只选择角色;最终授权必须用原登录账号 / 原修订并在 SQL 提交点复核。
// 请求体里的 sender_player_id 一律被覆盖,target / peer 只是会话选择参数。
package session

import (
	"context"

	base "proto/common/base"
	"shared/roleidentity"
)

// GetSessionDetails 返回入口准入后的客户端会话副本;没有会话时返回 false。
func GetSessionDetails(ctx context.Context) (*base.SessionDetails, bool) {
	return roleidentity.Session(ctx)
}

// WithSessionDetails 把会话放进 ctx。生产只由入口准入放(验签原件);业务逻辑单测用它造客户端来源。
func WithSessionDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return roleidentity.WithSession(ctx, detail)
}

// PlayerID 返回会话里的 player_id。没有会话、或会话里 player_id 为 0(未登录完成的会话)
// 都返回 false —— 逻辑层据此回 kInvalidParameter,绝不拿 0 当发言人写进历史。
func PlayerID(ctx context.Context) (uint64, bool) {
	detail, ok := GetSessionDetails(ctx)
	if !ok || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}
