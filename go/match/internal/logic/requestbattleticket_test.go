package logic

// 丢票补签 RequestBattleTicket(turn-based §18 D25;改道 client-rpc-router.md D33)的 miniredis 测试:
// 成功透传 / 无会话身份 / 观战索引不存在 / battle 节点未发现 / RPC 出错 / battle 拒签原样透传。
// battle 节点的 IssueBattleTicket 用 issueBattleTicketFn 顶替(与 spectate_test 的 stubObserverRPCs 同模式)。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/stretchr/testify/require"
)

// fakeIssueTicketRPC 记录 match 对 battle 节点发出的补签 RPC。
type fakeIssueTicketRPC struct {
	mu        sync.Mutex
	endpoints []string
	reqs      []*battlepb.IssueBattleTicketRequest
	// onIssue 决定响应;nil = 成功签出一张 PARTICIPANT 票。
	onIssue func(req *battlepb.IssueBattleTicketRequest) (*battlepb.IssueBattleTicketResponse, error)
}

func stubIssueTicketRPC(t *testing.T) *fakeIssueTicketRPC {
	t.Helper()
	f := &fakeIssueTicketRPC{}
	prev := issueBattleTicketFn
	issueBattleTicketFn = func(endpoint string, req *battlepb.IssueBattleTicketRequest) (*battlepb.IssueBattleTicketResponse, error) {
		f.mu.Lock()
		f.endpoints = append(f.endpoints, endpoint)
		f.reqs = append(f.reqs, req)
		onIssue := f.onIssue
		f.mu.Unlock()
		if onIssue != nil {
			return onIssue(req)
		}
		return &battlepb.IssueBattleTicketResponse{
			Assignment: &battlepb.BattleAssignedS2C{
				BattleId:       req.GetBattleId(),
				Host:           "10.0.0.7",
				Port:           30007,
				TokenPayload:   []byte("payload"),
				TokenSignature: []byte("sig"),
				ExpireAtMs:     nowMs() + 60_000,
				Role:           battlepb.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT,
			},
		}, nil
	}
	t.Cleanup(func() { issueBattleTicketFn = prev })
	return f
}

func (f *fakeIssueTicketRPC) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func requestTicket(t *testing.T, ctx context.Context, svcCtx *svc.ServiceContext, battleId uint64) *battlepb.RequestBattleTicketResponse {
	t.Helper()
	resp, err := NewRequestBattleTicketLogic(ctx, svcCtx).RequestBattleTicket(&battlepb.RequestBattleTicketRequest{
		BattleId: battleId,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func sessionCtx(playerId uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: playerId, SessionId: 42})
}

// 成功路径:会话身份 → 索引取 node 7 → 打到 newGatherSvcCtx 灌的 "battle-7" → assignment 原样透传。
func TestRequestBattleTicketIssuesViaIndexedBattleNode(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)
	const player = uint64(9101)
	const battleId = uint64(990001)
	registerBattle(t, svcCtx, battleId, nowMs(), "甲")

	resp := requestTicket(t, sessionCtx(player), svcCtx, battleId)
	require.Nil(t, resp.GetErrorMessage(), "tip=%v", resp.GetErrorMessage())
	require.NotNil(t, resp.GetAssignment())
	require.Equal(t, battleId, resp.GetAssignment().GetBattleId())
	require.Equal(t, "10.0.0.7", resp.GetAssignment().GetHost())
	require.Equal(t, uint32(30007), resp.GetAssignment().GetPort())
	require.Equal(t, battlepb.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT, resp.GetAssignment().GetRole())

	require.Equal(t, 1, fake.calls())
	require.Equal(t, []string{"battle-7"}, fake.endpoints, "必须打到观战索引记录的 battle 节点")
	require.Equal(t, battleId, fake.reqs[0].GetBattleId())
	require.Equal(t, player, fake.reqs[0].GetPlayerId(), "player_id 必须来自会话身份")
}

// 无会话身份:fail-closed,不读索引不发 RPC。
func TestRequestBattleTicketRejectsWithoutSession(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)
	registerBattle(t, svcCtx, 990002, nowMs(), "甲")

	resp := requestTicket(t, context.Background(), svcCtx, 990002)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, []string{"缺少玩家身份"}, resp.GetErrorMessage().GetParameters())
	require.Nil(t, resp.GetAssignment())
	require.Zero(t, fake.calls(), "没有身份不得向 battle 发任何请求")

	// 会话在但 player_id=0 同样拒绝。
	resp = requestTicket(t, sessionCtx(0), svcCtx, 990002)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Zero(t, fake.calls())
}

// 观战索引不存在(房间已结束 / 从未登记):kInvalidParameter,与 battle 侧"房间不存在"同 tip。
func TestRequestBattleTicketRoomIndexMissing(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)

	resp := requestTicket(t, sessionCtx(9103), svcCtx, 990003)
	require.Equal(t, uint32(table.CommonError_kInvalidParameter), resp.GetErrorMessage().GetId())
	require.Nil(t, resp.GetAssignment())
	require.Zero(t, fake.calls())
}

// 索引指向的 battle 节点不在 etcd 镜像里:kServiceUnavailable,不发 RPC。
func TestRequestBattleTicketBattleNodeNotDiscovered(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)
	const battleId = uint64(990004)
	// node 99 从未 Upsert 进 BattleNodes。
	registerSpectateBattle(svcCtx, battleId, 99, matchpb.MatchMode_MATCH_MODE_1V1, 0, []string{"甲"}, nowMs())

	resp := requestTicket(t, sessionCtx(9104), svcCtx, battleId)
	require.Equal(t, uint32(table.CommonError_kServiceUnavailable), resp.GetErrorMessage().GetId())
	require.Nil(t, resp.GetAssignment())
	require.Zero(t, fake.calls())
}

// RPC 出错(连不上 / 超时):kServiceUnavailable,不伪装成 battle 的裁决。
func TestRequestBattleTicketRPCError(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)
	fake.onIssue = func(*battlepb.IssueBattleTicketRequest) (*battlepb.IssueBattleTicketResponse, error) {
		return nil, errors.New("rpc error: code = DeadlineExceeded")
	}
	registerBattle(t, svcCtx, 990005, nowMs(), "甲")

	resp := requestTicket(t, sessionCtx(9105), svcCtx, 990005)
	require.Equal(t, uint32(table.CommonError_kServiceUnavailable), resp.GetErrorMessage().GetId())
	require.Nil(t, resp.GetAssignment())
	require.Equal(t, 1, fake.calls())
}

// battle 拒签(玩家不在名单):error_message 原样透传、assignment 为空,match 不改写裁决。
func TestRequestBattleTicketPassesThroughBattleRejection(t *testing.T) {
	svcCtx, _ := newGatherSvcCtx(t)
	fake := stubIssueTicketRPC(t)
	fake.onIssue = func(*battlepb.IssueBattleTicketRequest) (*battlepb.IssueBattleTicketResponse, error) {
		return &battlepb.IssueBattleTicketResponse{
			ErrorMessage: &base.TipInfoMessage{Id: uint32(table.CommonError_kInvalidParameter)},
		}, nil
	}
	registerBattle(t, svcCtx, 990006, nowMs(), "甲")

	resp := requestTicket(t, sessionCtx(9106), svcCtx, 990006)
	require.Equal(t, uint32(table.CommonError_kInvalidParameter), resp.GetErrorMessage().GetId())
	require.Nil(t, resp.GetAssignment())
	require.Equal(t, 1, fake.calls())
}
