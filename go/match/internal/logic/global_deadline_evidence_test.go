package logic

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
)

// 2026-11-06:截止作废取证(global_deadline_evidence.go)的 logic 层回归。分类规则是纯函数,逐条表格核对;与原实例的交互复用
// global_no_room_abort_test.go 的回环桩 battle(按真实 battle 的语义回 DestroyBattle / QueryBattleCreation)。不起 TiDB、
// 不依赖墙钟,也不靠「连不上的端口多久报错」这类与操作系统有关的时序:连得上但不是 battle 的端点用一个没注册任何服务的
// gRPC 服务器代替,期限已到用取消了的 ctx。时间判据与锁内裁决在 authority/deadline_evidence_test.go。

// 归类逐条:原实例亲口作答时以它为准(注册中心说什么都不作数);原地址上换了实例、注册中心里原 node_id 换了实例算原进程已不在;
// 连不上时只有「当场失败 + 镜像同步过 + 原实例已不在册」才算;其余一律判不了,由硬上限兜底。
func TestClassifyDeadlineEvidenceTable(t *testing.T) {
	f := &pb.MatchFormationRecord{BattleId: 3201, OperationId: "deadline-op", TargetInstanceId: "original-instance", TargetNodeId: 9}
	reached := func(state battlepb.BattleCreationState, destroyErr error) originalInstanceAnswer {
		return originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "original-instance", State: state}, destroyErr: destroyErr}
	}
	refused := errors.New("fixture: connection refused")
	mismatch := errors.New("fixture: placement_operation_mismatch")
	unreachable := originalInstanceAnswer{destroyErr: refused, queryErr: refused}
	otherInstance := originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{}, destroyErr: mismatch}
	listed := battleRegistryView{synced: true, listed: true}
	gone := battleRegistryView{synced: true}
	for _, c := range []struct {
		why      string
		answer   originalInstanceAnswer
		ctxDone  bool
		registry battleRegistryView
		want     authority.DeadlineAbortEvidence
	}{
		{"原实例立了墓碑、回 ABORTED", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED, nil), false, listed, authority.DeadlineEvidenceNoRoom},
		{"回 ABORTED 但本次立墓碑没成", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED, mismatch), false, listed, authority.DeadlineEvidenceUndetermined},
		{"原实例回 PRESENT", reached(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT, nil), false, listed, authority.DeadlineEvidenceBattleStillHolds},
		{"原实例回 PRESENT,注册中心却说已换", reached(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT, nil), false, battleRegistryView{synced: true, replaced: true}, authority.DeadlineEvidenceBattleStillHolds},
		{"原实例记着别的 operation(UNKNOWN)", reached(battlepb.BattleCreationState_BATTLE_CREATION_UNKNOWN, mismatch), false, gone, authority.DeadlineEvidenceUndetermined},
		{"原实例回 ABSENT", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABSENT, nil), false, listed, authority.DeadlineEvidenceUndetermined},
		{"原地址上应答的是另一个实例", otherInstance, false, listed, authority.DeadlineEvidenceInstanceGone},
		{"回显了别的实例(battle 不会这样回)", originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "someone-else"}}, false, gone, authority.DeadlineEvidenceUndetermined},
		{"注册中心里原 node_id 已是另一个实例", unreachable, false, battleRegistryView{synced: true, listed: true, replaced: true}, authority.DeadlineEvidenceInstanceGone},
		{"注册中心说已换,取证超时也算", unreachable, true, battleRegistryView{replaced: true}, authority.DeadlineEvidenceInstanceGone},
		{"连不上、仍在册", unreachable, false, listed, authority.DeadlineEvidenceUndetermined},
		{"连不上、已不在册", unreachable, false, gone, authority.DeadlineEvidenceInstanceGone},
		{"连不上、镜像没同步过", unreachable, false, battleRegistryView{}, authority.DeadlineEvidenceUndetermined},
		{"等到期限才失败、已不在册", unreachable, true, gone, authority.DeadlineEvidenceUndetermined},
	} {
		require.Equal(t, c.want, classifyDeadlineEvidence(f, c.answer, c.ctxDone, c.registry), c.why)
	}
}

// startBareGRPC 起一个没注册任何服务的 gRPC 服务器:连得上,但任何 battle 方法都当场回 Unimplemented(原地址上已不是 battle)。
func startBareGRPC(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	addr := listener.Addr().String()
	t.Cleanup(func() {
		discovery.RemoveEndpointConn(addr)
		server.Stop()
	})
	return addr
}

// 与原实例的交互和提前作废是同一次:先 DestroyBattle、再 QueryBattleCreation。没有房间 → 立墓碑并得到 NoRoom;房间还在 →
// DestroyBattle 被忽略、得到 BattleStillHolds,不伤还在等提交的房间;原地址上换了实例 → DestroyBattle 被拒也照样查询,
// 查询回空包,得到 InstanceGone。
func TestDeadlineEvidenceProbeAsksTheOriginalInstanceOnce(t *testing.T) {
	ctx := context.Background()
	registry := battleRegistryView{synced: true, listed: true}
	probe := deadlineEvidenceProbe(askOriginalInstance, func(*pb.MatchFormationRecord) battleRegistryView { return registry })

	calls := &noRoomCalls{}
	empty, endpoint := startNoRoomBattle(t, calls)
	require.Equal(t, authority.DeadlineEvidenceNoRoom, probe(ctx, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs)))
	require.Equal(t, []string{"DestroyBattle", "QueryBattleCreation"}, calls.snapshot(), "先立墓碑、再查询")
	require.True(t, empty.tombstoned(noRoomOperation))

	held, heldEndpoint := startNoRoomBattle(t, &noRoomCalls{})
	held.rooms[noRoomOperation] = true
	require.Equal(t, authority.DeadlineEvidenceBattleStillHolds, probe(ctx, noRoomFormation(t, heldEndpoint, noRoomInstance, noRoomNowMs)))
	require.False(t, held.tombstoned(noRoomOperation), "房间还在:DestroyBattle 被忽略")

	replacedCalls := &noRoomCalls{}
	_, replacedEndpoint := startNoRoomBattle(t, replacedCalls)
	require.Equal(t, authority.DeadlineEvidenceInstanceGone, probe(ctx, noRoomFormation(t, replacedEndpoint, "replaced-instance", noRoomNowMs)))
	require.Equal(t, []string{"DestroyBattle", "QueryBattleCreation"}, replacedCalls.snapshot(), "DestroyBattle 被拒也照样查询")
}

// 原地址上已不是 battle(方法当场失败):镜像同步过且原实例已不在册才算原进程已不在;仍在册、镜像没同步过都判不了;
// 原 node_id 已登记成另一个实例算。期限已到(ctx 已取消)时不算「原地址确认不了」。
func TestDeadlineEvidenceProbeWhenTheEndpointIsNotABattle(t *testing.T) {
	f := noRoomFormation(t, startBareGRPC(t), noRoomInstance, noRoomNowMs)
	for _, c := range []struct {
		why      string
		registry battleRegistryView
		want     authority.DeadlineAbortEvidence
	}{
		{"仍在册", battleRegistryView{synced: true, listed: true}, authority.DeadlineEvidenceUndetermined},
		{"镜像没同步过", battleRegistryView{}, authority.DeadlineEvidenceUndetermined},
		{"已不在册", battleRegistryView{synced: true}, authority.DeadlineEvidenceInstanceGone},
		{"原 node_id 已是另一个实例", battleRegistryView{synced: true, listed: true, replaced: true}, authority.DeadlineEvidenceInstanceGone},
	} {
		view := c.registry
		probe := deadlineEvidenceProbe(askOriginalInstance, func(*pb.MatchFormationRecord) battleRegistryView { return view })
		ctx, cancel := context.WithTimeout(context.Background(), authority.DeadlineProbeTimeout)
		require.Equal(t, c.want, probe(ctx, f), c.why)
		cancel()
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	probe := deadlineEvidenceProbe(askOriginalInstance, func(*pb.MatchFormationRecord) battleRegistryView { return battleRegistryView{synced: true} })
	require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(expired, f), "期限已到:没问成,不算证据")
}

// 注册中心镜像:stable 与 canary 两份都查;没同步过不算同步(Upsert 只是测试灌节点);原 node_id 登记成别的实例即「已换」。
// 没装配镜像(单测)时不给任何证据。
func TestBattleRegistryOfReadsBothMirrors(t *testing.T) {
	f := &pb.MatchFormationRecord{TargetNodeId: 9, TargetInstanceId: "original-instance"}
	require.Equal(t, battleRegistryView{listed: true}, battleRegistryOf(&svc.ServiceContext{}, f), "没装配镜像:当作仍在册")

	stable := discovery.NewNodeWatcher("battle", "BattleNodeService.rpc/", nil, nil)
	canary := discovery.NewNodeWatcher("battle-canary", "BattleNodeService.rpc/", nil, nil)
	s := &svc.ServiceContext{BattleNodes: stable, CanaryBattleNodes: canary}
	require.Equal(t, battleRegistryView{}, battleRegistryOf(s, f), "镜像没同步过、查不到")

	canary.Upsert("BattleNodeService.rpc/9", discovery.NodeEntry{NodeId: 9, NodeUuid: "original-instance", Endpoint: "10.0.0.9:7001"})
	require.Equal(t, battleRegistryView{listed: true}, battleRegistryOf(s, f), "canary 镜像里的原实例也认")
	require.False(t, stable.Synced(), "测试灌节点不等于从 etcd 同步过")

	stable.Upsert("BattleNodeService.rpc/9b", discovery.NodeEntry{NodeId: 9, NodeUuid: "replacement-instance", Endpoint: "10.0.0.9:7002"})
	view := battleRegistryOf(s, f)
	require.True(t, view.replaced, "原 node_id 已登记成另一个实例")
	require.True(t, view.listed)
}
