package roleidentity

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 归属预检的拒绝原因,与入口准入共用 online_entry_rejected_total 的 reason label(低基数)。
const (
	rejectOwnershipChanged    = "ownership_changed"
	rejectOwnershipUnverified = "ownership_unverified"
)

// OwnershipInterceptor 是入口准入之后的原角色归属预检(docs/design/gate-online-identity.md §9.3),装在 EntryInterceptor 之后。
//
// 入口准入只证明「这是哪条连接、哪个账号、哪次归属」。角色此刻是否仍归该账号,写路径在提交点持 role_fence 行锁复核;
// 读路径没有提交点。读方法不在业务里自己核归属的服务(guild、match、trade)装这一层:对每个带原身份的客户端请求
// 调一次 check。check 只读本库的护栏副本、不加锁,不替代提交点的复核。
//
//   - ctx 里没有原身份(内部调用、登记的匿名只读例外):直接放行,不调 check。
//   - check 回 nil:放行。
//   - rejected(err) 为真:确定查到角色已不归这份原身份,PermissionDenied。客户端驱动(过户、删角后旧连接还在发请求),只计数。
//   - 其余错误:目录读不到或库故障,这一次判不了,Aborted,客户端可以重试。这里只读、确定没写,
//     不回 Unavailable,免得把排空证明打成 Hold;写者闸门冻结由外层 serverbase 改写。
//
// check 与 rejected 缺一即返回错误,调用方拒启:少了哪一个预检都不成立,不能带着半截预检运行。
func OwnershipInterceptor(check func(context.Context) error, rejected func(error) bool) (grpc.UnaryServerInterceptor, error) {
	if check == nil || rejected == nil {
		return nil, errors.New("原角色归属预检缺判定函数")
	}
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if _, ok := Original(ctx); !ok {
			return next(ctx, request)
		}
		if err := check(ctx); err != nil {
			if rejected(err) {
				entryRejectedTotal.Inc(rejectOwnershipChanged)
				return nil, status.Error(codes.PermissionDenied, "role no longer belongs to this session identity")
			}
			entryRejectedTotal.Inc(rejectOwnershipUnverified)
			logx.WithContext(ctx).Infof("[online-entry] Ownership check inconclusive for %s: %v", info.FullMethod, err)
			return nil, status.Error(codes.Aborted, "role ownership not verified, retry")
		}
		return next(ctx, request)
	}, nil
}
