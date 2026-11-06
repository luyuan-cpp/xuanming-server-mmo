package main

// stress_match_queue.go —— 压测 join_queue 动作的匹配入口(ai.MatchQueue 的实现;服务器缺口清单第 10 条,
// docs/design/global-match-hierarchical-pools.md §11 R0「robot 加 JoinQueue 压测动作」)。
//
// 走与 battle-smoke 相同的两步:ensureNetworkProbe 保证本会话持有探测结果(按玩家缓存,临近过期才重测),
// 再经大厅连接发 JoinQueue,只等它自己的回包(smokeCaller 按请求号认领)。撤单按请求号与票号一起撤:
// 结局未知的入队服务端也撤得掉,并永久挡住它迟到入队(proto/match/match_service.proto CancelQueueRequest)。
//
// 成局之后机器人不连 battle:按池撮合的压测关心的是排队深度、成局与冲突,战斗按回合超时自己结束;
// RobotAI 在这局结束(NotifyBattleEnd)之前不再入队。

import (
	"errors"
	"fmt"
	"time"

	"proto/common/base"
	"proto/match"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/ai"
	"robot/metrics"
	"robot/pkg"
)

// stressMatchReplyTimeout 是 JoinQueue / CancelQueue 各自等回包的预算,同 networkProbeRPCTimeout 的取值理由
// (gate 等下游满 12s 回带请求号的 1003,15s 内回包或信封总会到)。
const stressMatchReplyTimeout = networkProbeRPCTimeout

type stressMatchQueue struct {
	gc      *pkg.GameClient
	stats   *metrics.Stats
	account string
	zoneID  uint32
	cfg     config.MatchQueueConfig
	mode    match.MatchMode
}

// newStressMatchQueue 只在 cfg 已校验过(config.MatchQueueConfig.Validate)之后调用。
func newStressMatchQueue(gc *pkg.GameClient, stats *metrics.Stats, account string, zoneID uint32, cfg config.MatchQueueConfig) *stressMatchQueue {
	return &stressMatchQueue{gc: gc, stats: stats, account: account, zoneID: zoneID, cfg: cfg, mode: cfg.MatchMode()}
}

// Join 实现 ai.MatchQueue。探测失败时请求还没发出,返回普通错误、凭据为空。
func (q *stressMatchQueue) Join() (ai.QueueTicket, error) {
	planID, err := ensureNetworkProbe(q.gc, q.stats, q.account)
	if err != nil {
		return ai.QueueTicket{}, fmt.Errorf("network probe: %w", err)
	}
	ticket := ai.QueueTicket{RequestID: chatSmokeRequestId()}
	request := &match.JoinQueueRequest{
		PlayerId:       q.gc.PlayerId,
		Mode:           q.mode,
		BattleConfigId: q.cfg.BattleConfigID,
		ZoneId:         q.zoneID,
		TeamSize:       q.cfg.TeamSize,
		RequestId:      ticket.RequestID,
		NetworkPlanId:  planID,
	}
	resp := &match.JoinQueueResponse{}
	caller := newSmokeCaller(q.gc, q.stats, q.account, stressMatchReplyTimeout, nil)
	envelopeTip, err := caller.call(game.MatchServiceJoinQueueMessageId, request, resp)
	if err != nil {
		// 本地没发出时服务端什么都没收到,其余(超时、断线、gate 推 kServiceUnavailable)都可能已受理。
		if errors.Is(err, pkg.ErrRPCNotSent) {
			return ai.QueueTicket{}, fmt.Errorf("JoinQueue: %w", err)
		}
		return ticket, fmt.Errorf("%w: JoinQueue: %v", ai.ErrMatchOutcomeUnknown, err)
	}
	if envelopeTip != 0 {
		// 1003 不只表示「没到 match」:路由 lane 在 match 的调用失败或超时后也回它,此时 match 可能已受理(同 battleSmokeJoinQueue)。
		// 别的信封码同样按结局未知处理:多撤一次没有副作用,少撤一次会留下一张撤不掉的票。
		return ticket, fmt.Errorf("%w: JoinQueue envelope rejected tip=%d", ai.ErrMatchOutcomeUnknown, envelopeTip)
	}
	if resp.GetErrorCode() != 0 || resp.GetErrorMessage().GetId() != 0 {
		// 「已在队列中」的拒绝带回现有票号:交给调用方到时撤掉,否则这个机器人会一直被拒。
		ticket.Ticket = resp.GetQueueTicket()
		if ticket.Ticket == "" {
			ticket = ai.QueueTicket{}
		}
		return ticket, fmt.Errorf("JoinQueue rejected error_code=%d tip=%d", resp.GetErrorCode(), resp.GetErrorMessage().GetId())
	}
	ticket.Ticket = resp.GetQueueTicket()
	return ticket, nil
}

// Cancel 实现 ai.MatchQueue。CancelQueue 的回包是空消息:没有这张票(已成局、已过期)服务端也照常回包。
func (q *stressMatchQueue) Cancel(t ai.QueueTicket) error {
	request := &match.CancelQueueRequest{PlayerId: q.gc.PlayerId, QueueTicket: t.Ticket, RequestId: t.RequestID}
	caller := newSmokeCaller(q.gc, q.stats, q.account, stressMatchReplyTimeout, nil)
	envelopeTip, err := caller.call(game.MatchServiceCancelQueueMessageId, request, &base.Empty{})
	if err != nil {
		return fmt.Errorf("CancelQueue: %w", err)
	}
	if envelopeTip != 0 {
		return fmt.Errorf("CancelQueue envelope rejected tip=%d", envelopeTip)
	}
	return nil
}

// stressMatchDwell 把配置里的撤单等待换成时长(只在校验过的配置上调用)。
func stressMatchDwell(cfg config.MatchQueueConfig) time.Duration {
	return time.Duration(cfg.DwellSeconds) * time.Second
}
