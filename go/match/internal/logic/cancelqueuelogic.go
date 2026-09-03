package logic

import (
	"context"

	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelQueueLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelQueueLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelQueueLogic {
	return &CancelQueueLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CancelQueue 取消排队。语义:
//   - 无 ticket:幂等成功(客户端重复取消 / ticket 已被开局收尾);
//   - ticket 不匹配:视为客户端持有陈旧票据,不动当前排队,幂等返回;
//   - 已进入 matched 及之后状态:取消太迟,凑单已在途,不可撤(此时
//     开局失败的补偿路径会把人送回队首或删票,不需要客户端参与);
//   - queued 态:先删 ticket 再 Lrem 出队。顺序有意如此 —— matcher 弹出
//     后会校验 ticket 存在性,先删票保证并发弹出方一定能识别出已取消。
//     删票是带 ticket id 且要求仍为 queued 的 Lua CAS(ticketCancelScript):
//     读票与删票之间 matcher 可能已把他弹出推进 matched,无条件 DEL 会让
//     "取消太迟"只停留在 Go 侧的一次读,玩家在"取消成功"后仍被冻结进战斗。
func (l *CancelQueueLogic) CancelQueue(in *matchpb.CancelQueueRequest) (*base.Empty, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		return &base.Empty{}, nil
	}

	ticket, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] CancelQueue 读 ticket 失败 player=%d: %v", playerId, err)
		return nil, err
	}
	if ticket == nil {
		// 幂等:没有在途排队,直接成功。
		return &base.Empty{}, nil
	}
	if in.QueueTicket != "" && in.QueueTicket != ticket.Ticket {
		l.Infof("[match] CancelQueue 票据不匹配,忽略 player=%d req=%s cur=%s",
			playerId, in.QueueTicket, ticket.Ticket)
		return &base.Empty{}, nil
	}
	if ticket.State != ticketStateQueued {
		// matched/ready:凑单或开局已在途,取消太迟。开局失败自有补偿路径。
		l.Infof("[match] CancelQueue 太迟,状态=%s player=%d", ticket.State, playerId)
		return &base.Empty{}, nil
	}

	deleted, err := cancelTicketIfQueued(l.svcCtx, playerId, ticket.Ticket)
	if err != nil {
		l.Errorf("[match] CancelQueue 删 ticket 失败 player=%d: %v", playerId, err)
		return nil, err
	}
	if !deleted {
		// 读票之后被 matcher 弹出(matched)或已被替换:取消太迟,凑单在途。
		l.Infof("[match] CancelQueue 太迟,票据已在读后被弹出或替换 player=%d ticket=%s", playerId, ticket.Ticket)
		return &base.Empty{}, nil
	}
	// 出队用票据里记录的 QueueKey(决策 D4);改 key 格式前写的旧票据没有
	// 该字段,新旧两种 key 各摘一次(旧 list 无 TTL,漏了就永久残留;
	// 见 keys.go legacyMatchQueueKey)。新格式 key 的 list 与评分镜像 ZSET 一条
	// 同 slot Lua 原子摘出(§11),旧格式 key 单 key LREM,集群安全。
	queueKeys := ticketQueueKeys(ticket)
	for _, queueKey := range queueKeys {
		if err := dequeueMember(l.svcCtx, queueKey, playerId); err != nil {
			// 出队失败留下的残留队列项会被 matcher 的票据校验丢弃,只记日志。
			l.Errorf("[match] CancelQueue 出队失败(残留由 matcher 票据校验兜底) player=%d queue=%s: %v",
				playerId, queueKey, err)
		}
	}
	l.Infof("[match] 取消排队成功 player=%d mode=%d config=%d queue=%v",
		playerId, ticket.Mode, ticket.Config, queueKeys)
	return &base.Empty{}, nil
}
