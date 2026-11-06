// Package session 存取入口准入放进 ctx 的客户端会话,并装配 friend 的原角色归属预检。
//
// 会话只有一个来源:roleidentity.EntryInterceptor 从 gate 签名的验签原件放进 ctx,且只给带完整原身份的
// 客户端请求放(docs/design/gate-online-identity.md §9.2)。本包不自己解 metadata。
//
// 哪些方法客户端能调由生成的客户端方法集合决定(shared/generated/clientrpc,与 gate 上行白名单同源),
// 不再手写名单。S2C 推送 NotifyFriendEvent 标了 CLIENT_RPC_PUSH,不在集合里:客户端带会话拨它在入口就被
// PermissionDenied,不靠 friend_server.go「今天返回 Unimplemented」兜底;RoleFenceParticipant 同理。
// friend 的请求 message 已经删掉了 player_id 字段,身份只能从会话来,没有回落到请求体的余地。
package session

import (
	"context"

	base "proto/common/base"
	"shared/roleidentity"
)

// WithDetails 把会话放进 ctx。生产只由入口准入放(验签原件);业务逻辑单测用它造客户端来源。
func WithDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return roleidentity.WithSession(ctx, detail)
}

// ClientPlayerID 返回客户端来源请求的 player_id。ok=false 表示没有会话(内部调用)或会话里 player_id 为 0。
func ClientPlayerID(ctx context.Context) (playerID uint64, ok bool) {
	detail, found := roleidentity.Session(ctx)
	if !found || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// Details 返回入口准入后的客户端会话副本;没有会话时返回 nil。
func Details(ctx context.Context) *base.SessionDetails {
	detail, _ := roleidentity.Session(ctx)
	return detail
}
