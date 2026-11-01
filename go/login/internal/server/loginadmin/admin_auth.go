package server

import (
	"context"
	"crypto/subtle"
	"strings"

	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// LoginAdmin 的鉴权。写法照 player_inbox 的 AdminAuthInterceptor。
//
// login 的 gRPC 端口对外接受未鉴权调用(登录入口本来如此),身份闸门只校验带了身份声明的调用,挡不住 LoginAdmin。
// 做成拦截器而不是方法开头各调一次:LoginAdmin 的全部方法都要口令,新增方法不可能忘记挂。

const (
	// AdminTokenEnv 是运维口令所在的环境变量。口令只经 Secret 注入,不进配置文件。
	AdminTokenEnv = "MMORPG_LOGIN_ADMIN_TOKEN"
	// AdminTokenMetadataKey 是口令所在的 gRPC metadata 键,必须全小写。
	AdminTokenMetadataKey = "x-admin-token"
	// MinAdminTokenBytes 是非空口令的最小长度,和 player_inbox 同值。
	MinAdminTokenBytes = 16
)

// adminMethodPrefix 由生成的服务描述派生,不手写服务名。
var adminMethodPrefix = "/" + loginpb.LoginAdmin_ServiceDesc.ServiceName + "/"

// AdminAuthInterceptor 返回 LoginAdmin 的鉴权拦截器。token 为空 = 运维面整体停用(fail-closed):每个方法都回 PermissionDenied,不是免鉴权。
// 比较用常数时间,口令值绝不进日志。
func AdminAuthInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, adminMethodPrefix) {
			return handler(ctx, req)
		}
		if err := authorizeAdmin(ctx, token, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func authorizeAdmin(ctx context.Context, want, method string) error {
	if want == "" {
		logx.WithContext(ctx).Errorf("[LoginAdmin] %s refused: %s is not set, the admin surface is disabled peer=%s", method, AdminTokenEnv, peerAddr(ctx))
		return status.Errorf(codes.PermissionDenied, "%s is disabled because %s is not set", method, AdminTokenEnv)
	}
	var got string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(AdminTokenMetadataKey); len(vals) > 0 {
			got = vals[0]
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		// 只记有没有带,绝不记口令本身。
		logx.WithContext(ctx).Errorf("[LoginAdmin] %s refused: bad or missing %s (present=%t) peer=%s", method, AdminTokenMetadataKey, got != "", peerAddr(ctx))
		return status.Errorf(codes.PermissionDenied, "%s requires a valid %s", method, AdminTokenMetadataKey)
	}
	return nil
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
