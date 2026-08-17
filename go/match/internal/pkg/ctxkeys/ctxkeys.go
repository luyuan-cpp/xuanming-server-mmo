// Package ctxkeys 存取 gate 经 gRPC metadata(x-session-detail-bin)附带的
// 会话详情。与 go/login/internal/logic/pkg/ctxkeys 同模式:权威 player_id
// 一律从这里取,请求体里的 player_id 仅供内部调用使用。
package ctxkeys

import (
	"context"

	base "proto/common/base"
)

// contextKey is a custom type to avoid key collisions.
type contextKey string

const (
	SessionDetailsKey contextKey = "SessionDetailsKey"
)

// GetSessionDetails retrieves SessionDetails from the context.
func GetSessionDetails(ctx context.Context) (*base.SessionDetails, bool) {
	v := ctx.Value(SessionDetailsKey)
	detail, ok := v.(*base.SessionDetails)
	return detail, ok
}

// WithSessionDetails stores SessionDetails in the context.
func WithSessionDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return context.WithValue(ctx, SessionDetailsKey, detail)
}
