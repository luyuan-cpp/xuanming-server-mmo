package logic

import (
	"testing"

	"match/internal/constants"

	"github.com/stretchr/testify/require"
)

// TestJoinQueueClearsStaleReadyTicket:上一局结束后(battle:lock 已被 scene 删除)
// 仍在 TTL 内的 ready 票据不得阻塞再次排队 —— 2026-09-02 跨 zone 冒烟复跑实测:
// 上一局 16s 前结束,ready 票据 TTL 60s 未到,JoinQueue 一直 ErrAlreadyQueued。
func TestJoinQueueClearsStaleReadyTicket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 5101, 1)
	stale := &queueTicket{Ticket: "stale-ready", Mode: 3, Config: 0, State: ticketStateReady, EnqueuedAtMs: 1}
	mustCreateTicket(t, svcCtx, 5101, stale)

	resp := joinQueue1v1(t, svcCtx, 5101)
	require.Zero(t, resp.ErrorCode, "已结束战斗的 ready 票据必须被清掉并放行")
	require.NotEqual(t, "stale-ready", resp.QueueTicket, "必须签发新票据")

	ticket, err := loadTicket(svcCtx, 5101)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, ticketStateQueued, ticket.State)
	require.Equal(t, resp.QueueTicket, ticket.Ticket)
}

// TestJoinQueueStillRejectsMatchedTicket:matched 态(gather 在途)仍然拒绝,
// 靠按组大小计算的 matched TTL 自愈,不能被"再排一次"抢先删掉。
func TestJoinQueueStillRejectsMatchedTicket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 5102, 2)
	inflight := &queueTicket{Ticket: "in-gather", Mode: 3, Config: 0, State: ticketStateMatched, EnqueuedAtMs: 1}
	mustCreateTicket(t, svcCtx, 5102, inflight)

	resp := joinQueue1v1(t, svcCtx, 5102)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	require.Equal(t, "in-gather", resp.QueueTicket)
}
