// Package session 存取 gate 经 gRPC metadata(x-session-detail-bin)附带的会话详情。
//
// 与 go/match/internal/pkg/ctxkeys、go/login 的 ctxkeys 同模式(契约 §3):
// chat 的**权威发言人 = 会话里的 player_id**,请求体里的 sender_player_id 一律被覆盖,
// 只有 target_player_id / peer_player_id 信请求体。
//
// 拦截器与读取函数放在同一个包里,是为了让"谁写 ctx key、谁读 ctx key"只有一处定义 ——
// 两边各写一份字符串 key,改一边另一边就静默取不到会话,所有请求都会变成"无会话"。
package session

import (
	"context"
	"encoding/base64"

	base "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// MetadataKey 是 gate 附带会话身份的 metadata 键,值 = base64(proto SessionDetails)。
// 与 C++ kSessionBinMetaKey、路由服 logic.SessionMetaKey 同一字符串;路由服原值透传。
const MetadataKey = "x-session-detail-bin"

// contextKey 用私有类型避免与其它包的 ctx key 撞名。
type contextKey string

const sessionDetailsKey contextKey = "SessionDetailsKey"

// GetSessionDetails 从 ctx 取出会话详情;拦截器没放进去(缺头 / 坏头)时返回 false。
func GetSessionDetails(ctx context.Context) (*base.SessionDetails, bool) {
	detail, ok := ctx.Value(sessionDetailsKey).(*base.SessionDetails)
	return detail, ok && detail != nil
}

// WithSessionDetails 把会话详情放进 ctx(拦截器与单测共用)。
func WithSessionDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return context.WithValue(ctx, sessionDetailsKey, detail)
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

// UnaryServerInterceptor 解出 x-session-detail-bin 放进 ctx。
//
// 缺头 / base64 坏 / proto 坏一律**放行不拒**(fail-open 到逻辑层):
//   - 拦截器只负责"解码",不负责"鉴权决策"——拒绝码与指标 outcome 由逻辑层统一给出,
//     否则同一种失败会在拦截器(gRPC status)和逻辑层(in-band tip)各有一种表现;
//   - 路由服对缺头已经回 Unauthenticated,走到这里还缺头说明是直连调试或上游 bug,
//     逻辑层回 kInvalidParameter 足够,且客户端能看到 tip 而不是断链。
//
// 坏头打 Error 日志(真实流量里不该出现,出现即 gate / 路由服版本问题),缺头不打(避免噪音)。
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return handler(ctx, req)
		}
		values := md.Get(MetadataKey)
		if len(values) == 0 {
			return handler(ctx, req)
		}
		bin, err := base64.StdEncoding.DecodeString(values[0])
		if err != nil {
			logx.WithContext(ctx).Errorf("[chat] %s base64 解码失败 method=%s: %v", MetadataKey, info.FullMethod, err)
			return handler(ctx, req)
		}
		detail := &base.SessionDetails{}
		if err := proto.Unmarshal(bin, detail); err != nil {
			logx.WithContext(ctx).Errorf("[chat] SessionDetails 反序列化失败 method=%s: %v", info.FullMethod, err)
			return handler(ctx, req)
		}
		return handler(WithSessionDetails(ctx, detail), req)
	}
}
