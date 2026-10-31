package logic

// CreateBattle 的回执分类(k8s-client-entry D82 与 SQL 终局协议):
//   - 节点级准入拒绝(gRPC Unavailable + "battle_not_allocatable")状态码与消息必须同时精确匹配,调用方据此短时熔断实例;
//   - 其余 RPC 失败结果未知;业务拒绝、回执不符、明确未采用协议 1 都不算建房成功。

import (
	"errors"
	"fmt"
	"testing"

	"match/internal/discovery"

	battlepb "proto/battle"

	"shared/generated/pb/table"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errNotAllocatable 是 battle 节点准入拒绝在 Go 客户端看到的原样 gRPC 错误。
var errNotAllocatable = status.Error(codes.Unavailable, battleNotAllocatableMessage)

// stubCreateBattle 把 createBattleFn 换成固定回复。
func stubCreateBattle(t *testing.T, reply func(req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error)) {
	t.Helper()
	prev := createBattleFn
	createBattleFn = func(_ string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return reply(req)
	}
	t.Cleanup(func() { createBattleFn = prev })
}

// isBattleNotAllocatable 纯判定:只认原样的 Unavailable + battle_not_allocatable。
func TestIsBattleNotAllocatable(t *testing.T) {
	require.True(t, isBattleNotAllocatable(errNotAllocatable))
	require.False(t, isBattleNotAllocatable(nil))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Unavailable, "battle_not_allocatable ")))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Unavailable, "BATTLE_NOT_ALLOCATABLE")))
	require.False(t, isBattleNotAllocatable(status.Error(codes.Internal, battleNotAllocatableMessage)))
	require.False(t, isBattleNotAllocatable(fmt.Errorf("wrap: %w", errNotAllocatable)))
	require.False(t, isBattleNotAllocatable(errors.New(battleNotAllocatableMessage)), "非 gRPC 错误")
	require.False(t, isBattleNotAllocatable(status.Error(codes.Unavailable, "transport is closing")), "连接断开时请求可能已送达")
}

func TestCreateBattleClassifiesReply(t *testing.T) {
	node := discovery.NodeEntry{NodeId: 7, Endpoint: "battle-7"}
	req := &battlepb.CreateBattleRequest{BattleId: 1, TargetInstanceId: "instance-a"}

	// 准入拒绝:errors.Is 可判。
	stubCreateBattle(t, func(*battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return nil, errNotAllocatable
	})
	require.ErrorIs(t, createBattle(node, req), errBattleNotAllocatable)

	// 普通 RPC 失败不误判成准入拒绝。
	stubCreateBattle(t, func(*battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return nil, status.Error(codes.Unavailable, "transport is closing")
	})
	err := createBattle(node, req)
	require.Error(t, err)
	require.NotErrorIs(t, err, errBattleNotAllocatable)

	// 业务拒绝(如 battle 预签票据失败,turn-based §22 D70)。
	stubCreateBattle(t, func(r *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return &battlepb.CreateBattleResponse{BattleId: r.BattleId, ErrorMessage: tipErr(uint32(table.CommonError_kServiceUnavailable), "拒绝建房")}, nil
	})
	require.Error(t, createBattle(node, req))

	// 回执的原局或原实例不符。
	stubCreateBattle(t, func(r *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return &battlepb.CreateBattleResponse{BattleId: r.BattleId + 1, TargetInstanceId: r.TargetInstanceId, AdoptedOutcomeProtocolVersion: 1}, nil
	})
	require.Error(t, createBattle(node, req))
	stubCreateBattle(t, func(r *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return &battlepb.CreateBattleResponse{BattleId: r.BattleId, TargetInstanceId: "instance-b", AdoptedOutcomeProtocolVersion: 1}, nil
	})
	require.Error(t, createBattle(node, req))

	// 明确未采用终局协议 1:带回实例与版本,调用方据此持久化观察结果,不追认。
	stubCreateBattle(t, func(r *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return &battlepb.CreateBattleResponse{BattleId: r.BattleId, TargetInstanceId: r.TargetInstanceId}, nil
	})
	var adoption *outcomeAdoptionError
	require.ErrorAs(t, createBattle(node, req), &adoption)
	require.Equal(t, "instance-a", adoption.instance)
	require.Zero(t, adoption.version)

	// 成功。
	stubCreateBattle(t, func(r *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return &battlepb.CreateBattleResponse{BattleId: r.BattleId, TargetInstanceId: r.TargetInstanceId, AdoptedOutcomeProtocolVersion: 1}, nil
	})
	require.NoError(t, createBattle(node, req))
}
