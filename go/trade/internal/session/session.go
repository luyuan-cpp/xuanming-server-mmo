// Package session 解码 gate(经 client_rpc_router 原样透传)附带的会话元数据
// x-session-detail-bin,并决定哪些 RPC 允许客户端来源调用。
//
// 照 go/guild/internal/session 的口径(P1-4、port-decisions D-9),不照 chat 的"坏头放行":
// trade 同一进程里挂着内部服务 TradeAdmin,chat 那种不按方法准入的拦截器会让带会话的请求
// 直接打到内部方法。规则:
//
//  1. 没有会话元数据 = 内部调用(robot 直连播种 / 运维工具),放行。客户端无法靠"不带会话"绕过:
//     路由服对缺会话的客户端消息直接拒绝,gate 直连模式又根本到不了 trade。
//  2. 带了会话元数据却解不开 = 拒绝(Unauthenticated)。gate 注入的元数据不会坏,坏了只可能是
//     篡改或版本错配,绝不能退化成"当内部调用处理"。
//  3. 客户端来源只放行 ClientMethods 白名单;其余一律 PermissionDenied。以后新增的 RPC 不写进
//     白名单就不会意外对客户端开放。
//
// 这是第二道防线:TradeAdmin 不标 OptionIsClientProtocolService、单独放在 trade_admin.proto,
// gate 与路由服本来就不会把它当客户端消息。
package session

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	tradepb "proto/trade"
)

// MetadataKey 是 gate 写入、路由服透传的会话元数据键,值 = base64(proto SessionDetails)。
const MetadataKey = "x-session-detail-bin"

type contextKey struct{}

// ClientMethods 是允许客户端来源调用的方法全集:ClientPlayerJubaozhai 的 4 个 P1 方法。
// TradeAdmin 的任何方法都刻意不在其中(session_test.go 遍历两个 ServiceDesc 守住)。
var ClientMethods = map[string]struct{}{
	tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName:   {},
	tradepb.ClientPlayerJubaozhai_GetListingDetail_FullMethodName: {},
	tradepb.ClientPlayerJubaozhai_SetFavorite_FullMethodName:      {},
	tradepb.ClientPlayerJubaozhai_GetMyShelf_FullMethodName:       {},
}

// WithDetails 把已校验的会话放进 ctx(拦截器与单测共用)。
func WithDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return context.WithValue(ctx, contextKey{}, detail)
}

// ClientPlayerID 返回客户端来源请求的权威 player_id。
// ok=false 表示没有会话:对 ClientPlayerJubaozhai 的方法,逻辑层据此回 kInvalidParameter ——
// 客户端请求体里本来就没有 player_id,不存在"内部调用信请求体"的分支。
func ClientPlayerID(ctx context.Context) (playerID uint64, ok bool) {
	detail, _ := ctx.Value(contextKey{}).(*base.SessionDetails)
	if detail == nil || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// UnaryServerInterceptor 校验会话元数据并按 clientMethods 做方法级准入。
// 放在 killswitch 之后、serverbase 之前:被关停的方法不必解码会话,
// 被本层拒绝的调用也不会进入业务耗时指标。
func UnaryServerInterceptor(clientMethods map[string]struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get(MetadataKey)
		if len(values) == 0 {
			return handler(ctx, req)
		}

		detail, err := decode(values[0])
		if err != nil {
			logx.WithContext(ctx).Errorf("[trade] 拒绝 %s:会话元数据无法解码: %v", info.FullMethod, err)
			return nil, status.Error(codes.Unauthenticated, "invalid session metadata")
		}
		if _, allowed := clientMethods[info.FullMethod]; !allowed {
			logx.WithContext(ctx).Errorf("[trade] 拒绝客户端调用内部方法 %s(session_id=%d)", info.FullMethod, detail.GetSessionId())
			return nil, status.Errorf(codes.PermissionDenied, "%s is not callable by clients", info.FullMethod)
		}
		return handler(WithDetails(ctx, detail), req)
	}
}

func decode(value string) (*base.SessionDetails, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	detail := &base.SessionDetails{}
	if err := proto.Unmarshal(raw, detail); err != nil {
		return nil, fmt.Errorf("unmarshal SessionDetails: %w", err)
	}
	if detail.GetPlayerId() == 0 {
		return nil, fmt.Errorf("SessionDetails without player_id (session_id=%d)", detail.GetSessionId())
	}
	return detail, nil
}
