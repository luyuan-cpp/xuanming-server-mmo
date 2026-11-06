package main

import (
	"context"
	"strings"

	"match/internal/pkg/ctxkeys"
	"shared/roleidentity"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 解码之后捕获原件；SQL目录只在真实事务首锁复核，不给旧会话反查新owner补权。
// 在线域原账号只认 gate 签名原件(callerauth 验签拦截器存进 ctx),不读 login Redis,所以本拦截器不依赖任何存储。
func originalRoleInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	detail, present := ctxkeys.GetSessionDetails(ctx)
	if !present {
		if strings.HasPrefix(info.FullMethod, "/match.MatchService/") || strings.HasPrefix(info.FullMethod, "/teampb.ClientPlayerTeam/") {
			return nil, status.Error(codes.PermissionDenied, "匹配客户端缺完整原会话")
		}
		return handler(ctx, req) // 内部方法各有独立调用方准入，不能制造客户端actor。
	}
	if detail.GetPlayerId() == 0 || detail.GetOwnerRevision() == 0 {
		return nil, status.Error(codes.PermissionDenied, "匹配缺原角色会话修订")
	}
	// 解码出的声明必须与验签原件逐字段一致且带合法账号;未签名、被改写或缺账号都按原会话失效拒绝。
	bound, err := roleidentity.CaptureSession(ctx, detail)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "匹配原登录会话已失效")
	}
	return handler(bound, req)
}
