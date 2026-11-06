// Package ctxkeys 存取入口准入放进 ctx 的客户端会话。
//
// 会话只有一个来源:roleidentity.EntryInterceptor 从 gate 签名的验签原件放进 ctx,且只给带完整原身份的
// 客户端请求放(docs/design/gate-online-identity.md §9.2)。本包只是 match 业务代码沿用的取值名字,
// 不自己解 metadata。权威 player_id 一律从这里取;没有会话就是内部调用,请求体里的 player_id 才生效。
package ctxkeys

import (
	"context"

	base "proto/common/base"
	"shared/roleidentity"
)

// GetSessionDetails 返回入口准入后的客户端会话副本;内部调用没有会话,返回 false。
func GetSessionDetails(ctx context.Context) (*base.SessionDetails, bool) {
	return roleidentity.Session(ctx)
}

// WithSessionDetails 把会话放进 ctx。生产只由入口准入放(验签原件);业务逻辑单测用它造客户端来源。
func WithSessionDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return roleidentity.WithSession(ctx, detail)
}
