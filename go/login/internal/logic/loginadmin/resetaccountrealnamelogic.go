package loginadminlogic

import (
	"context"
	"errors"

	"login/internal/logic/pkg/accountdeletion"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	"proto/login"
	"shared/accountdirectory"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ResetAccountRealNameLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewResetAccountRealNameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResetAccountRealNameLogic {
	return &ResetAccountRealNameLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ResetAccountRealName 客服重置实名(docs/design/real-name-anti-addiction.md §5.2 末)。鉴权在拦截器(x-admin-token)。
// 客服先在系统外核实身份;这里清掉账号上的 pi 和认证中的提交,玩家下次登录重新实名。幂等。
// 已经在线的会话不动:截止时刻与上报记录在绑定时就定了,下一次登录按新的实名判定。
func (l *ResetAccountRealNameLogic) ResetAccountRealName(in *login.ResetAccountRealNameRequest) (*login.ResetAccountRealNameResponse, error) {
	if (in.GetPlayerId() == 0) == (in.GetAccount() == "") || !boundedText(in.GetOperator(), 64) || !boundedText(in.GetReason(), 256) {
		return nil, status.Error(codes.InvalidArgument,
			"exactly one of player_id / account, plus operator (<= 64 bytes) and reason (<= 256 bytes), are required")
	}
	realNames, ok := l.svcCtx.RealNames()
	if !ok {
		return nil, status.Error(codes.Unavailable, "account directory is not ready")
	}
	notFound := &login.ResetAccountRealNameResponse{ErrorMessage: &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}}
	account := in.GetAccount()
	if account == "" {
		owned, err := l.svcCtx.AccountDirectory.Lookup(l.ctx, in.GetPlayerId())
		if errors.Is(err, accountdirectory.ErrMissing) {
			return notFound, nil
		}
		if err != nil {
			return nil, status.Error(codes.Unavailable, "account directory lookup failed")
		}
		account = owned.GetAccount()
	}
	had, err := realNames.ResetRealName(l.ctx, account)
	if errors.Is(err, accountdirectory.ErrMissing) || errors.Is(err, accountdirectory.ErrInvalid) {
		return notFound, nil
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "reset real name failed")
	}
	// 认证中的那次提交也作废,免得之后查到结果把旧身份绑回去。删不掉只记日志:它 48 小时内自己过期。
	if err := l.svcCtx.LoginRedis.Del(l.ctx, redisslot.RealNamePendingKey(account)).Err(); err != nil {
		l.Errorf("[real-name] admin reset could not drop the pending submission: account=%s err=%v", accountdeletion.MaskAccount(account), err)
	}
	l.Infof("[real-name] admin reset: account=%s had_real_name=%v operator=%s reason=%q peer=%s",
		accountdeletion.MaskAccount(account), had, in.GetOperator(), in.GetReason(), peerAddrOf(l.ctx))
	return &login.ResetAccountRealNameResponse{HadRealName: had}, nil
}
