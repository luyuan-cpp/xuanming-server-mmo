// Package session 解码 gate(经 client_rpc_router 原样透传)附带的会话元数据
// x-session-detail-bin,并决定哪些 RPC 允许客户端来源调用。
//
// 口径照 go/trade/internal/session 与 go/guild/internal/session(port-decisions D-9),
// **不照 chat 的"坏头放行"**。三条规则:
//
//  1. 没有会话元数据 = 内部调用(robot 直连播种 / 运维工具),放行。客户端无法靠"不带会话"
//     绕过本层:路由服对缺会话的客户端消息直接拒绝(forwardlogic.go missing_session),
//     而 friend 只承诺路由服模式、没有 gate 直连白名单(D-12),客户端根本到不了这里。
//  2. 带了会话元数据却解不开 = 拒绝(Unauthenticated)。gate 注入的元数据不会坏,坏了只可能是
//     篡改或版本错配,绝不能退化成"当内部调用处理、信请求体里的 player_id" —— 那等于把伪造
//     身份的口子重新打开。friend 的请求 message 已经删掉了 player_id 字段,连回落的余地都不留。
//  3. 客户端来源只放行 ClientMethods 白名单;其余一律 PermissionDenied。
//
// # friend 为什么也需要方法级白名单
//
// friend 不像 trade 那样在同一进程里挂内部服务(TradeAdmin),表面上"整个 ClientPlayerFriend
// 都是客户端协议"、白名单似乎多余。但本期给同一个客户端服务加了 S2C 推送方法
// NotifyFriendEvent —— 它必须写在服务名含 ClientPlayer 的服务里,生成器才会给 Unity / robot
// 出下行 handler;代价是它同时出现在服务端的 ServiceDesc 上,成为一个"客户端可拨"的方法名。
// 服务端不提供 NotifyFriendEvent 的 C2S 语义(friend_server.go 刻意不实现,继承
// UnimplementedClientPlayerFriendServer),但"今天返回 Unimplemented"不是安全保证:
// 将来谁手滑实现了它,就是一个凭空伪造"XX 通过了你的好友申请"事件的入口。
// 所以在会话层按方法名挡住,并由 session_test.go 遍历 ServiceDesc 守住"新增 RPC 必须表态"。
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
	friendpb "proto/friend"
)

// MetadataKey 是 gate 写入、路由服透传的会话元数据键,值 = base64(proto SessionDetails)。
const MetadataKey = "x-session-detail-bin"

type contextKey struct{}

// ClientMethods 是允许客户端来源调用的方法全集:ClientPlayerFriend 的 10 个 C2S 方法。
//
// S2C 推送方法 NotifyFriendEvent 刻意不在其中(理由见包注释)。这张表是白名单而不是黑名单:
// 以后新增的 RPC 不写进来就不会意外对客户端开放,写不写都得在 session_test.go 里表态。
var ClientMethods = map[string]struct{}{
	friendpb.ClientPlayerFriend_AddFriend_FullMethodName:          {},
	friendpb.ClientPlayerFriend_AcceptFriend_FullMethodName:       {},
	friendpb.ClientPlayerFriend_RejectFriend_FullMethodName:       {},
	friendpb.ClientPlayerFriend_RemoveFriend_FullMethodName:       {},
	friendpb.ClientPlayerFriend_GetFriendList_FullMethodName:      {},
	friendpb.ClientPlayerFriend_GetPendingRequests_FullMethodName: {},
	friendpb.ClientPlayerFriend_Block_FullMethodName:              {},
	friendpb.ClientPlayerFriend_Unblock_FullMethodName:            {},
	friendpb.ClientPlayerFriend_ListBlocks_FullMethodName:         {},
	friendpb.ClientPlayerFriend_RecommendFriends_FullMethodName:   {},
}

// WithDetails 把已校验的会话放进 ctx(拦截器与单测共用)。
func WithDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return context.WithValue(ctx, contextKey{}, detail)
}

// ClientPlayerID 返回客户端来源请求的权威 player_id(D-9:"我"只从会话取,目标 id 才看请求体)。
//
// ok=false 表示这次调用没带会话。逻辑层据此回 constants.ErrInvalidParameter 的 in-band 响应:
// 请求 message 里已经没有 player_id 字段,不存在"内部调用信请求体"的分支。
// 刻意**不用** kPlayerNotFoundInSession —— 那是 Tip.xlsx fault 列为 1 的故障码,
// 会把"客户端没带身份"刷成服务端故障告警。
func ClientPlayerID(ctx context.Context) (playerID uint64, ok bool) {
	detail, _ := ctx.Value(contextKey{}).(*base.SessionDetails)
	if detail == nil || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// UnaryServerInterceptor 校验会话元数据并按 clientMethods 做方法级准入。
//
// 在一元链里放在 killswitch 之后、serverbase 之前:被关停的方法不必解码会话,
// 被本层拒绝的调用也不会污染业务耗时与 in-band 码指标。
func UnaryServerInterceptor(clientMethods map[string]struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get(MetadataKey)
		if len(values) == 0 {
			return handler(ctx, req)
		}

		detail, err := decode(values[0])
		if err != nil {
			logx.WithContext(ctx).Errorf("[friend] 拒绝 %s:会话元数据无法解码: %v", info.FullMethod, err)
			return nil, status.Error(codes.Unauthenticated, "invalid session metadata")
		}
		if _, allowed := clientMethods[info.FullMethod]; !allowed {
			logx.WithContext(ctx).Errorf("[friend] 拒绝客户端调用非客户端方法 %s(session_id=%d)",
				info.FullMethod, detail.GetSessionId())
			return nil, status.Errorf(codes.PermissionDenied, "%s is not callable by clients", info.FullMethod)
		}
		return handler(WithDetails(ctx, detail), req)
	}
}

// decode 解出会话并做最低限度的完整性检查。
// player_id == 0 也算坏头:后续所有逻辑都拿它当权威身份,放过去等于让一个 0 号玩家进业务。
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
