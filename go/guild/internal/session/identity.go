package session

import (
	"context"

	"shared/roleidentity"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OriginalIdentityInterceptor 在入口准入之后,对客户端请求做一次原角色归属预检(check 读本库护栏副本、不加锁)。
// 入口准入已把验签原件里的原账号、原修订与角色捕获进 ctx;取不到原件就是内部调用或匿名只读例外,直接放行。
// 它是 guild 读方法唯一的归属检查;写方法在提交点还会持副本行锁按同一份原件复核。
// roleRejected 判 check 的错误是不是确实查到角色变了或被冻结。
func OriginalIdentityInterceptor(check func(context.Context) error, roleRejected func(error) bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if _, ok := roleidentity.Original(ctx); !ok {
			return next(ctx, req)
		}
		if check == nil {
			return nil, status.Error(codes.Unauthenticated, "原登录身份验证依赖未就绪")
		}
		if err := check(ctx); err != nil {
			if roleRejected != nil && roleRejected(err) {
				return nil, status.Error(codes.PermissionDenied, "角色归属已变化或被冻结")
			}
			// 目录读不到、库故障:这次判不了,客户端要能重试,不能告诉它角色变了。这里只读,确定没写,不回 Unavailable 免得打成排空 Hold。
			// 闸门冻结由外面的 serverbase 改写。
			logx.WithContext(ctx).Infof("[guild] original identity check inconclusive: %v", err)
			return nil, status.Error(codes.Aborted, "role identity not verified, retry")
		}
		return next(ctx, req)
	}
}
