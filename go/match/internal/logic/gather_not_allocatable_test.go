package logic

// CreateBattle 被 battle 节点级准入拒绝(gRPC Unavailable + "battle_not_allocatable",
// k8s-client-entry D82)时 gather 的换节点重试:
//   - 精确组合 → 不发 DestroyBattle,换一个没试过的节点重试**一次**;
//   - 重试成功 → 正常开局,观战索引记实际建房的节点;
//   - 仍被拒 / 没有其他节点 → 干净失败(不发 DestroyBattle),按 not_allocatable 补偿;
//   - 重试时遇到其他错误 → 回到通用路径,只对重试节点 DestroyBattle;
//   - 状态码或消息任一不精确匹配 → 原有通用路径不变(DestroyBattle 兜底、不重试)。
// scene RPC 经 stubGatherRPCs 顶替;CreateBattle / DestroyBattle 在其上再换成按调用序
// 决定结果、并记下目标节点的记录器。

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errNotAllocatable 是 battle 节点准入拒绝在 Go 客户端看到的原样 gRPC 错误。
var errNotAllocatable = status.Error(codes.Unavailable, battleNotAllocatableMessage)

// battleRPCCall 一次打到 battle 节点的 CreateBattle / DestroyBattle。
type battleRPCCall struct {
	endpoint string
	battleId uint64
}

// battleRPCRecorder 按调用序决定 CreateBattle 结果,并记下每次 CreateBattle / DestroyBattle 的目标节点。
type battleRPCRecorder struct {
	mu        sync.Mutex
	creates   []battleRPCCall
	destroys  []battleRPCCall
	createErr func(call int) error // call 从 0 起;返回 nil = 建房成功
}

func (r *battleRPCRecorder) createCalls() []battleRPCCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]battleRPCCall(nil), r.creates...)
}

func (r *battleRPCRecorder) destroyCalls() []battleRPCCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]battleRPCCall(nil), r.destroys...)
}

// stubBattleCreateDestroy 必须在 stubGatherRPCs 之后调用:覆盖其 createBattleFn / destroyBattleFn,
// Cleanup 先还原成 stubGatherRPCs 的桩,再由 stubGatherRPCs 的 Cleanup 还原生产实现。
func stubBattleCreateDestroy(t *testing.T, createErr func(call int) error) *battleRPCRecorder {
	t.Helper()
	r := &battleRPCRecorder{createErr: createErr}
	prevCreate, prevDestroy := createBattleFn, destroyBattleFn
	createBattleFn = func(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		r.mu.Lock()
		call := len(r.creates)
		r.creates = append(r.creates, battleRPCCall{endpoint: endpoint, battleId: req.GetBattleId()})
		r.mu.Unlock()
		if err := r.createErr(call); err != nil {
			return nil, err
		}
		return &battlepb.CreateBattleResponse{BattleId: req.GetBattleId()}, nil
	}
	destroyBattleFn = func(endpoint string, req *battlepb.DestroyBattleRequest) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.destroys = append(r.destroys, battleRPCCall{endpoint: endpoint, battleId: req.GetBattleId()})
		return nil
	}
	t.Cleanup(func() { createBattleFn, destroyBattleFn = prevCreate, prevDestroy })
	return r
}

// addBattleNode 往 battle 池再灌一个节点(newGatherSvcCtx 已有 node 7 = "battle-7")。
func addBattleNode(svcCtx *svc.ServiceContext, nodeId uint32) {
	svcCtx.BattleNodes.Upsert(fmt.Sprintf("%s%d", svc.BattleNodeRpcPrefix, nodeId),
		discovery.NodeEntry{NodeId: nodeId, Endpoint: fmt.Sprintf("battle-%d", nodeId)})
}

// nodeIdOfEndpoint 把测试节点的 endpoint("battle-<id>")还原成 node_id。
func nodeIdOfEndpoint(t *testing.T, endpoint string) uint32 {
	t.Helper()
	var id uint32
	_, err := fmt.Sscanf(endpoint, "battle-%d", &id)
	require.NoError(t, err)
	return id
}

// failFirstN 前 n 次 CreateBattle 回 err,之后建房成功。
func failFirstN(n int, err error) func(int) error {
	return func(call int) error {
		if call < n {
			return err
		}
		return nil
	}
}

// requireRequeuedInOrder 断言队列凑单失败补偿后两人票据仍是原票、回到 queued,并按弹出序回队首。
func requireRequeuedInOrder(t *testing.T, svcCtx *svc.ServiceContext, queueList func() []string,
	members []uint64, tickets map[uint64]string,
) {
	t.Helper()
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket, "节点不可分配不是玩家的错,不得删票 player=%d", pid)
		require.Equal(t, tickets[pid], ticket.Ticket, "回队首的是原票据 player=%d", pid)
		require.Equal(t, ticketStateQueued, ticket.State, "不得推进 ready player=%d", pid)
	}
	want := make([]string, 0, len(members))
	for _, pid := range members {
		want = append(want, fmt.Sprintf("%d", pid))
	}
	require.Equal(t, want, queueList(), "按弹出序回队首")
}

// 首选节点准入拒绝 → 换另一个节点重试成功:不发 DestroyBattle,正常开局,观战索引记实际建房节点。
func TestGatherNotAllocatableRetriesOnAnotherNode(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	addBattleNode(svcCtx, 8)
	fake := stubGatherRPCs(t, map[uint64]string{9701: "fp", 9702: "fp"})
	battles := stubBattleCreateDestroy(t, failFirstN(1, errNotAllocatable))
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9701, 9702)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	successBase := metrics.GatherValue(mode, "success")
	notAllocBase := metrics.GatherValue(mode, "not_allocatable")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	creates := battles.createCalls()
	require.Len(t, creates, 2, "被拒后恰好重试一次")
	require.NotEqual(t, creates[0].endpoint, creates[1].endpoint, "必须换一个没试过的节点")
	require.ElementsMatch(t, []string{"battle-7", "battle-8"}, []string{creates[0].endpoint, creates[1].endpoint})
	require.Equal(t, creates[0].battleId, creates[1].battleId, "重试沿用同一个 battle_id / 请求")
	require.Empty(t, battles.destroyCalls(), "准入拒绝无副作用,不得发 DestroyBattle")
	require.Empty(t, fake.cancelled, "开局成功,不解冻")

	record, err := loadSpectateBattle(svcCtx, creates[1].battleId)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, nodeIdOfEndpoint(t, creates[1].endpoint), record.GetBattleNodeId(),
		"观战索引(补签定位)必须指向实际建房的节点,而不是首选节点")
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateReady, ticket.State)
	}
	require.Equal(t, successBase+1, metrics.GatherValue(mode, "success"))
	require.Equal(t, notAllocBase, metrics.GatherValue(mode, "not_allocatable"))
}

// 两个节点都准入拒绝 → 干净失败:两次 CreateBattle 打在不同节点、零 DestroyBattle,
// 两人解冻、原票回队首,不登记观战索引,记 not_allocatable。
func TestGatherNotAllocatableTwiceFailsCleanWithoutDestroy(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	addBattleNode(svcCtx, 8)
	fake := stubGatherRPCs(t, map[uint64]string{9711: "fp", 9712: "fp"})
	battles := stubBattleCreateDestroy(t, failFirstN(2, errNotAllocatable))
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9711, 9712)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	notAllocBase := metrics.GatherValue(mode, "not_allocatable")
	createFailedBase := metrics.GatherValue(mode, "create_failed")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	creates := battles.createCalls()
	require.Len(t, creates, 2)
	require.NotEqual(t, creates[0].endpoint, creates[1].endpoint)
	require.Empty(t, battles.destroyCalls(), "两次都是准入拒绝,房间必未建成,不发 DestroyBattle")
	require.ElementsMatch(t, members, fake.cancelled, "已冻结的两人都要解冻")
	record, err := loadSpectateBattle(svcCtx, creates[0].battleId)
	require.NoError(t, err)
	require.Nil(t, record, "未建成的战斗不得登记观战索引")
	require.Zero(t, activeIndexSize(t, svcCtx))
	requireRequeuedInOrder(t, svcCtx, func() []string {
		list, err := mr.List(queueKey)
		require.NoError(t, err)
		return list
	}, members, tickets)
	require.Equal(t, notAllocBase+1, metrics.GatherValue(mode, "not_allocatable"))
	require.Equal(t, createFailedBase, metrics.GatherValue(mode, "create_failed"), "准入拒绝不记成 create_failed")
}

// 池里只有一个节点:被拒后没有可换的节点 → 不重试、不发 DestroyBattle,直接补偿(切磋无票据)。
func TestRunChallengeGatherNotAllocatableSingleNodeFailsClean(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	battles := stubBattleCreateDestroy(t, failFirstN(1, errNotAllocatable))
	const challenger, responder = uint64(9721), uint64(9722)
	setPlayerLocation(t, mr, challenger, 1)
	setPlayerLocation(t, mr, responder, 2)
	mode := matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE.String()
	notAllocBase := metrics.GatherValue(mode, "not_allocatable")

	require.False(t, RunChallengeGather(svcCtx, 0, challenger, responder))

	creates := battles.createCalls()
	require.Len(t, creates, 1, "没有其他节点可换,不得回头再试同一个节点")
	require.Equal(t, "battle-7", creates[0].endpoint)
	require.Empty(t, battles.destroyCalls())
	require.ElementsMatch(t, []uint64{challenger, responder}, fake.cancelled)
	require.False(t, mr.Exists(matchTicketKey(challenger)), "切磋从未入队,补偿不得凭空写票")
	require.False(t, mr.Exists(matchTicketKey(responder)))
	require.Equal(t, notAllocBase+1, metrics.GatherValue(mode, "not_allocatable"))
}

// 三个节点都会拒绝:只重试一次(共两次 CreateBattle),不会把整个池试一遍。
func TestGatherNotAllocatableRetriesOnlyOnce(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	addBattleNode(svcCtx, 8)
	addBattleNode(svcCtx, 9)
	stubGatherRPCs(t, map[uint64]string{9731: "fp", 9732: "fp"})
	battles := stubBattleCreateDestroy(t, func(int) error { return errNotAllocatable })
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9731, 9732)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	creates := battles.createCalls()
	require.Len(t, creates, 2, "D82:换节点只重试一次")
	require.NotEqual(t, creates[0].endpoint, creates[1].endpoint)
	require.Empty(t, battles.destroyCalls())
}

// 重试节点返回其他错误(如超时):回到通用路径 —— 只对重试节点 DestroyBattle(首选节点
// 准入拒绝无副作用,不碰它),确认销毁后按 create_failed 补偿。
func TestGatherNotAllocatableThenOtherErrorDestroysOnlyRetriedNode(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	addBattleNode(svcCtx, 8)
	fake := stubGatherRPCs(t, map[uint64]string{9741: "fp", 9742: "fp"})
	battles := stubBattleCreateDestroy(t, func(call int) error {
		if call == 0 {
			return errNotAllocatable
		}
		return status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	})
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9741, 9742)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	createFailedBase := metrics.GatherValue(mode, "create_failed")
	notAllocBase := metrics.GatherValue(mode, "not_allocatable")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	creates := battles.createCalls()
	require.Len(t, creates, 2)
	destroys := battles.destroyCalls()
	require.Len(t, destroys, 1, "只有可能已建房的重试节点需要兜底销毁")
	require.Equal(t, creates[1].endpoint, destroys[0].endpoint)
	require.Equal(t, creates[1].battleId, destroys[0].battleId)
	require.ElementsMatch(t, members, fake.cancelled)
	requireRequeuedInOrder(t, svcCtx, func() []string {
		list, err := mr.List(queueKey)
		require.NoError(t, err)
		return list
	}, members, tickets)
	require.Equal(t, createFailedBase+1, metrics.GatherValue(mode, "create_failed"))
	require.Equal(t, notAllocBase, metrics.GatherValue(mode, "not_allocatable"))
}

// 状态码与消息必须同时精确匹配:只有 Unavailable(连接断开 / 对端重启也会回)、只有消息、
// 被包装过的状态错误,都按原有通用路径走 —— 不重试、DestroyBattle 兜底、记 create_failed。
func TestGatherInexactNotAllocatableKeepsGenericPath(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"Unavailable 但消息不同", status.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing\"")},
		{"消息相同但状态码不同", status.Error(codes.ResourceExhausted, battleNotAllocatableMessage)},
		{"被包装的准入拒绝", fmt.Errorf("代理层: %w", errNotAllocatable)},
		{"非 gRPC 错误", errors.New(battleNotAllocatableMessage)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			addBattleNode(svcCtx, 8)
			fake := stubGatherRPCs(t, map[uint64]string{9751: "fp", 9752: "fp"})
			battles := stubBattleCreateDestroy(t, failFirstN(1, tc.err))
			members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9751, 9752)
			mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
			createFailedBase := metrics.GatherValue(mode, "create_failed")

			RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

			creates := battles.createCalls()
			require.Len(t, creates, 1, "非精确匹配不重试")
			destroys := battles.destroyCalls()
			require.Len(t, destroys, 1, "非精确匹配时房间可能已建成,必须 DestroyBattle 兜底")
			require.Equal(t, creates[0].endpoint, destroys[0].endpoint)
			require.ElementsMatch(t, members, fake.cancelled)
			require.Equal(t, createFailedBase+1, metrics.GatherValue(mode, "create_failed"))
		})
	}
}

// isBattleNotAllocatable 纯判定:只认原样的 Unavailable + battle_not_allocatable。
func TestIsBattleNotAllocatable(t *testing.T) {
	require.True(t, isBattleNotAllocatable(errNotAllocatable))
	require.False(t, isBattleNotAllocatable(nil))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Unavailable, "battle_not_allocatable ")))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Unavailable, "BATTLE_NOT_ALLOCATABLE")))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Internal, battleNotAllocatableMessage)))
	require.False(t, isBattleNotAllocatable(fmt.Errorf("wrap: %w", errNotAllocatable)))

	// createBattle 的包装对调用方可判(errors.Is),且不把普通 RPC 失败误判成准入拒绝。
	stubGatherRPCs(t, nil)
	stubBattleCreateDestroy(t, func(call int) error {
		if call == 0 {
			return errNotAllocatable
		}
		return status.Error(codes.Unavailable, "transport is closing")
	})
	node := discovery.NodeEntry{NodeId: 7, Endpoint: "battle-7"}
	req := &battlepb.CreateBattleRequest{BattleId: 1}
	require.ErrorIs(t, createBattle(node, req), errBattleNotAllocatable)
	err := createBattle(node, req)
	require.Error(t, err)
	require.NotErrorIs(t, err, errBattleNotAllocatable)
}
