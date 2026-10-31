package main

import (
	"context"
	"strings"

	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/loginsession"
	"shared/roleidentity"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 解码之后捕获原件；SQL目录只在真实事务首锁复核，不给旧会话反查新owner补权。
func originalRoleInterceptor(sc *svc.ServiceContext) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
		account, err := loginsession.Account(ctx, sc.LoginSessions, detail)
		if err != nil {
			return nil, status.Error(codes.PermissionDenied, "匹配原登录会话已失效")
		}
		bound, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: detail.OwnerRevision, Role: &base.AccountSimplePlayer{PlayerId: detail.PlayerId}})
		if err != nil {
			return nil, status.Error(codes.PermissionDenied, "匹配原角色身份不完整")
		}
		return handler(bound, req)
	}
}
