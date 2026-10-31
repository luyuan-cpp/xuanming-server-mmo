package logic

import (
	"context"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"

	"github.com/zeromicro/go-zero/core/logx"
)

type RequestBattleTicketLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRequestBattleTicketLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RequestBattleTicketLogic {
	return &RequestBattleTicketLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RequestBattleTicket 丢票补签(turn-based §18 D25;改道见 client-rpc-router.md D33):
//   - player_id 只取 gate 注入的会话身份(x-session-detail-bin → ctxkeys);请求体没有也不该有
//     player_id。没有会话身份即不可信来源,fail-closed;
//   - 落点只读 SQL 成局原件(globalRequestBattleTicket),Redis 投影丢失不构成「战斗已结束」的证据;
//   - battle 核对名单并自签,match 不持票据密钥、不复制名单;battle 的 error_message / assignment 原样透传。
//
// 为什么走 match 而不是 gate→battle 直达:gate 两种路由模式都不中继 battle 消息(D33;
// turn-based §22 D66),而票据发放必须由已鉴权的
// 大厅会话背书 —— match 是客户端协议里唯一同时掌握"会话身份 + 房间所在节点"的服务。
func (l *RequestBattleTicketLogic) RequestBattleTicket(in *battlepb.RequestBattleTicketRequest) (*battlepb.RequestBattleTicketResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[ticket] RequestBattleTicket missing session identity battle=%d", in.GetBattleId())
		metrics.ObserveRequestBattleTicket("no_session")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	resp, err := globalRequestBattleTicket(l.ctx, l.svcCtx, detail.GetPlayerId(), in.GetBattleId())
	switch {
	case err != nil:
		metrics.ObserveRequestBattleTicket("internal")
	case resp.GetErrorMessage().GetId() != 0:
		metrics.ObserveRequestBattleTicket("rejected")
	default:
		metrics.ObserveRequestBattleTicket("ok")
	}
	return resp, err
}
