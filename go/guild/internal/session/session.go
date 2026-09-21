// Package session 解码 gate(经 client_rpc_router 原样透传)附带的会话元数据
// x-session-detail-bin,并决定哪些 RPC 允许客户端来源调用。
//
// 元数据约定与 go/match/internal/pkg/ctxkeys 相同(base64 编码的 SessionDetails),
// 但多两条 match 没有的规矩,来自 docs/design/xuanming-port-decisions-20260910.md D-9:
//
//  1. **带了会话元数据却解不开 = 拒绝**(Unauthenticated)。gate 注入的元数据不会坏,
//     坏了只可能是篡改或版本错配;绝不能退化成"当内部调用处理、信请求体里的 player_id",
//     那等于把伪造身份的口子重新打开。
//  2. **客户端来源只放行白名单方法**。proto 的 OptionIsClientProtocolService 是服务级开关,
//     UpdateGuildScore 这类东西向方法会跟着进 gate 白名单和路由表;在这里默认拒绝,
//     以后新增的 RPC 不写进 ClientMethods 就不会意外对客户端开放。
//
// 没有会话元数据的调用视为内部调用(GM / 运维工具 / 其它服务):路由服对缺会话的客户端
// 消息直接拒绝(forwardlogic.go missing_session),gate 直连模式又根本到不了 guild,
// 所以客户端无法通过"不带会话"绕过本层。
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
	pb "proto/guild"
)

// MetadataKey 是 gate 写入、路由服透传的会话元数据键。
const MetadataKey = "x-session-detail-bin"

type contextKey struct{}

// ClientMethods 是允许客户端来源调用的方法全集,**逐个显式登记**。
//
// 这是一道 fail-closed 的安全边界:新增 RPC 不写进本表就调不到,而不是默认放开。
// 两类方法刻意不在其中:
//   - UpdateGuildScore:公会积分只能由服务端结算写入,放给客户端就是任意改榜。
//   - Notify*(NotifyGuildChanged 等):它们只是给推送分配 message id 的占位 RPC,
//     下行走 Kafka,**永远不进本表**;客户端发这个 id 只可能是在试探。
var ClientMethods = map[string]struct{}{
	pb.GuildService_CreateGuild_FullMethodName:             {},
	pb.GuildService_GetGuild_FullMethodName:                {},
	pb.GuildService_GetPlayerGuild_FullMethodName:          {},
	pb.GuildService_LeaveGuild_FullMethodName:              {},
	pb.GuildService_DisbandGuild_FullMethodName:            {},
	pb.GuildService_SetAnnouncement_FullMethodName:         {},
	pb.GuildService_SetGuildMemberRole_FullMethodName:      {},
	pb.GuildService_KickGuildMember_FullMethodName:         {},
	pb.GuildService_TransferGuildLeader_FullMethodName:     {},
	pb.GuildService_ApplyJoinGuild_FullMethodName:          {},
	pb.GuildService_CancelGuildApplication_FullMethodName:  {},
	pb.GuildService_ListMyGuildApplications_FullMethodName: {},
	pb.GuildService_ListGuildApplications_FullMethodName:   {},
	pb.GuildService_ReviewGuildApplication_FullMethodName:  {},
	pb.GuildService_GetGuildRank_FullMethodName:            {},
	pb.GuildService_GetGuildRankByGuild_FullMethodName:     {},
	// 帮会经济(B5a 登记协议与准入;实现在 B5b,此前回 Unimplemented)。
	pb.GuildService_GetGuildDonateOptions_FullMethodName: {},
	pb.GuildService_DonateToGuild_FullMethodName:         {},
	pb.GuildService_UpgradeGuild_FullMethodName:          {},
	pb.GuildService_GetGuildShop_FullMethodName:          {},
	pb.GuildService_BuyGuildShopGoods_FullMethodName:     {},
}

// WithDetails 把已校验的会话放进 ctx。
func WithDetails(ctx context.Context, detail *base.SessionDetails) context.Context {
	return context.WithValue(ctx, contextKey{}, detail)
}

// ClientPlayerID 返回客户端来源请求的权威 player_id。
// ok=false 表示这是一次内部调用(没有会话),调用方沿用请求体里的字段。
func ClientPlayerID(ctx context.Context) (playerID uint64, ok bool) {
	detail, _ := ctx.Value(contextKey{}).(*base.SessionDetails)
	if detail == nil || detail.GetPlayerId() == 0 {
		return 0, false
	}
	return detail.GetPlayerId(), true
}

// UnaryServerInterceptor 校验会话元数据并按 clientMethods 做方法级准入。
// 放在 killswitch 之后、serverbase 之前:被关停的方法不必解码会话,
// 而被本层拒绝的调用也不会进入业务耗时指标。
func UnaryServerInterceptor(clientMethods map[string]struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get(MetadataKey)
		if len(values) == 0 {
			return handler(ctx, req)
		}

		detail, err := decode(values[0])
		if err != nil {
			logx.Errorf("[guild] 拒绝 %s:会话元数据无法解码: %v", info.FullMethod, err)
			return nil, status.Error(codes.Unauthenticated, "invalid session metadata")
		}
		if _, allowed := clientMethods[info.FullMethod]; !allowed {
			logx.Errorf("[guild] 拒绝客户端调用内部方法 %s(session_id=%d)", info.FullMethod, detail.GetSessionId())
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
