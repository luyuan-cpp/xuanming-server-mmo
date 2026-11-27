package loginpregatelogic

import (
	"context"
	"errors"

	"login/internal/logic/pkg/passportsession"
	"login/internal/svc"
	"proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type HandlePassportEventLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewHandlePassportEventLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HandlePassportEventLogic {
	return &HandlePassportEventLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// HandlePassportEvent 处理网关转来的官方通行证回调(docs/design/official-passport-registration.md §3.7)。
// 回调只用来找人,按通行证此刻的状态吊销游戏令牌并踢线(passportsession.HandleEvent),幂等。
// 回调格式不对回 InvalidArgument(网关回 400,通行证不必重投);没配管理凭据回 FailedPrecondition;
// 核对不了或吊销失败回 Unavailable(网关回 503,通行证按重投设置再来)。
func (l *HandlePassportEventLogic) HandlePassportEvent(in *login.PassportEventRequest) (*login.PassportEventResponse, error) {
	if l.svcCtx.PassportSessions == nil {
		return nil, status.Error(codes.FailedPrecondition, "passport admin client is not configured")
	}
	checked, err := l.svcCtx.PassportSessions.HandleEvent(l.ctx, in.GetPayload())
	if errors.Is(err, passportsession.ErrBadEvent) {
		l.Infof("[passport] webhook payload rejected: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		l.Errorf("[passport] webhook handling failed, the passport will redeliver: checked=%d: %v", checked, err)
		return nil, status.Error(codes.Unavailable, "passport webhook handling failed")
	}
	return &login.PassportEventResponse{CheckedUsers: uint32(checked)}, nil
}
