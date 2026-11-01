package session

import (
	"context"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/loginsession"
	"shared/roleidentity"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func Details(ctx context.Context) *base.SessionDetails {
	detail, _ := ctx.Value(contextKey{}).(*base.SessionDetails)
	if detail == nil {
		return nil
	}
	return proto.Clone(detail).(*base.SessionDetails)
}

// 原账号只读部署显式的全局login实例；修订只读gate发出时的会话原件，不从当前归属补造。
// roleRejected 判 check 的错误是不是确实查到角色变了或被冻结。
func OriginalIdentityInterceptor(login loginsession.Reader, check func(context.Context) error, roleRejected func(error) bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		detail := Details(ctx)
		if detail == nil {
			return next(ctx, req)
		}
		if detail.GetOwnerRevision() == 0 || check == nil {
			return nil, status.Error(codes.Unauthenticated, "原登录修订未就绪")
		}
		account, err := loginsession.Account(ctx, login, detail)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "原登录态不可用")
		}
		ctx, err = roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: detail.OwnerRevision, Role: &base.AccountSimplePlayer{PlayerId: detail.PlayerId}})
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "原登录身份非法")
		}
		if err = check(ctx); err != nil {
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
