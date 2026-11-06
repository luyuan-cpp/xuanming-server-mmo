package session

import (
	"context"

	"shared/roleidentity"

	"google.golang.org/grpc"
)

// 解码后捕获原账号与原wire修订。在线域原账号只认 gate 签名原件，不读 login Redis；
// 未签名、与原件不符或会话不完整时不绑身份照常放行，不补当前目录，由业务入口按缺原件拒绝。
func OriginalIdentityInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		detail, ok := GetSessionDetails(ctx)
		if !ok {
			return next(ctx, req)
		}
		bound, err := roleidentity.CaptureSession(ctx, detail)
		if err != nil {
			return next(ctx, req)
		}
		return next(bound, req)
	}
}
