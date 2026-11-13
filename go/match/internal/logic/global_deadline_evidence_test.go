package logic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
)

// 2026-11-06:截止作废取证(global_deadline_evidence.go)的 logic 层回归。分类规则是纯函数,逐条表格核对;与原实例的交互复用
// global_no_room_abort_test.go 的回环桩 battle(按真实 battle 的语义回 DestroyBattle / QueryBattleCreation)。不起 TiDB、
// 不依赖墙钟:涉及真实拨号的用例只断言结果,期限给 30 秒(Windows 回环拒连要经 SYN 重试约 2 秒才报,远在其内),
// 不断言「多久报错」;期限已到用取消了的 ctx。时间判据与锁内裁决在 authority/deadline_evidence_test.go。

// generousProbeContext 是涉及真实拨号的用例用的期限:只为不让用例挂死,断言不依赖它多长。
func generousProbeContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// neverDial 是不该走到拒连拨号的用例给的 refuses:走到就判用例失败。
func neverDial(t *testing.T) func(context.Context, string) bool {
	return func(context.Context, string) bool {
		t.Errorf("unexpected TCP refusal dial")
		return false
	}
}

// witnessOf 是只有拒连拨号与固定镜像两样旁证的装配,不查 Pod(2026-11-11 之前的全部旁证)。
func witnessOf(refuses func(context.Context, string) bool, view battleRegistryView) originalInstanceWitness {
	return originalInstanceWitness{refuses: refuses, registry: func(*pb.MatchFormationRecord) battleRegistryView { return view }}
}

// closedLoopbackAddr 返回一个刚关掉、没人在听的回环端口:拨它对端当场回 RST。
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	t.Cleanup(func() { discovery.RemoveEndpointConn(addr) })
	return addr
}

// 归类逐条:原实例亲口作答时以它为准(注册中心说什么都不作数);原地址上换了实例算原进程已不在;查询没成时只有
// 「原地址 TCP 当场拒连 + 镜像同步过 + 原实例已不在册」才算;其余一律判不了,由硬上限兜底。
// 评审修正(2026-11-06)的回归:查询失败本身(哪怕是在期限之内当场失败)不再算「连不上」—— 缓存的 gRPC 连接停在
// TRANSIENT_FAILURE 时 RPC 当场回 Unavailable,黑洞端点从第二次取证起就是这样;「原 node_id 已登记成另一个实例」也不再单独算。
func TestClassifyDeadlineEvidenceTable(t *testing.T) {
	f := &pb.MatchFormationRecord{BattleId: 3201, OperationId: "deadline-op", TargetInstanceId: "original-instance", TargetNodeId: 9}
	reached := func(state battlepb.BattleCreationState, destroyErr error) originalInstanceAnswer {
		return originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "original-instance", State: state}, destroyErr: destroyErr}
	}
	unavailable := errors.New("fixture: rpc error: code = Unavailable")
	mismatch := errors.New("fixture: placement_operation_mismatch")
	failed := originalInstanceAnswer{destroyErr: unavailable, queryErr: unavailable}
	otherInstance := originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{}, destroyErr: mismatch}
	listed := battleRegistryView{synced: true, listed: true}
	gone := battleRegistryView{synced: true}
	for _, c := range []struct {
		why      string
		answer   originalInstanceAnswer
		refused  bool
		registry battleRegistryView
		podGone  bool
		want     authority.DeadlineAbortEvidence
	}{
		{"原实例立了墓碑、回 ABORTED", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED, nil), false, listed, false, authority.DeadlineEvidenceNoRoom},
		{"回 ABORTED 但本次立墓碑没成", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED, mismatch), false, listed, false, authority.DeadlineEvidenceUndetermined},
		{"只查询、没立墓碑的 ABORTED 不算没有房间", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED, errTombstoneNotAttempted), false, listed, false, authority.DeadlineEvidenceUndetermined},
		{"原实例回 PRESENT", reached(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT, nil), false, listed, false, authority.DeadlineEvidenceBattleStillHolds},
		{"原实例回 PRESENT,镜像里却已不在册", reached(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT, nil), false, gone, false, authority.DeadlineEvidenceBattleStillHolds},
		{"原实例回 PRESENT,Pod 查询却说没有(命名空间配错):以原实例为准", reached(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT, nil), false, gone, true, authority.DeadlineEvidenceBattleStillHolds},
		{"原实例记着别的 operation(UNKNOWN)", reached(battlepb.BattleCreationState_BATTLE_CREATION_UNKNOWN, mismatch), false, gone, false, authority.DeadlineEvidenceUndetermined},
		{"原实例回 ABSENT", reached(battlepb.BattleCreationState_BATTLE_CREATION_ABSENT, nil), false, listed, false, authority.DeadlineEvidenceUndetermined},
		{"原地址上应答的是另一个实例", otherInstance, false, listed, false, authority.DeadlineEvidenceInstanceGone},
		{"回显了别的实例(battle 不会这样回)", originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "someone-else"}}, false, gone, false, authority.DeadlineEvidenceUndetermined},
		{"回显了别的实例,Pod 查询说没有也不算(连上的是谁说不清)", originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "someone-else"}}, false, gone, true, authority.DeadlineEvidenceUndetermined},
		{"查询失败、原地址拒连、已不在册", failed, true, gone, false, authority.DeadlineEvidenceInstanceGone},
		{"查询失败、原地址拒连、仍在册(停机排空:已停听、还没撤租约)", failed, true, listed, false, authority.DeadlineEvidenceUndetermined},
		{"查询失败、原地址拒连、镜像没同步过", failed, true, battleRegistryView{}, false, authority.DeadlineEvidenceUndetermined},
		{"缓存连接当场回 Unavailable、原地址没拒连、已不在册(黑洞端点的第二次取证)", failed, false, gone, false, authority.DeadlineEvidenceUndetermined},
		{"超时或黑洞、已不在册(含原 node_id 已被新进程占用的分区)", failed, false, gone, false, authority.DeadlineEvidenceUndetermined},
		{"查询失败、仍在册", failed, false, listed, false, authority.DeadlineEvidenceUndetermined},
		{"超时或黑洞、仍在册,K8s 里原 IP 上已没有存活的 Pod(Agones 下崩溃)", failed, false, listed, true, authority.DeadlineEvidenceInstanceGone},
		{"查询失败、镜像没同步过,K8s 里原 IP 上已没有存活的 Pod", failed, false, battleRegistryView{}, true, authority.DeadlineEvidenceInstanceGone},
	} {
		require.Equal(t, c.want, classifyDeadlineEvidence(f, c.answer, c.refused, c.registry, c.podGone), c.why)
	}
	require.True(t, gone.forgotten())
	require.False(t, listed.forgotten())
	require.False(t, battleRegistryView{}.forgotten(), "镜像没同步过:不算不在册")
}

// 拒连只认 TCP 的 RST:端口上没人听才算;连得上(不论对端是谁)、期限已到、空地址都不算;只认错误码,不认错误文案。
func TestOriginalAddressRefusesOnlyOnTcpReset(t *testing.T) {
	ctx := generousProbeContext(t)
	open, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = open.Close() })
	require.False(t, originalAddressRefuses(ctx, open.Addr().String()), "连得上:不算拒连")

	closed := closedLoopbackAddr(t)
	require.True(t, originalAddressRefuses(ctx, closed), "端口上没人听:对端当场回 RST")

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, originalAddressRefuses(expired, closed), "期限已到:没拨成,不算拒连")
	require.False(t, originalAddressRefuses(ctx, ""))

	require.True(t, connectionRefused(fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)))
	require.False(t, connectionRefused(context.DeadlineExceeded))
	require.False(t, connectionRefused(fmt.Errorf("dial tcp: %w", syscall.ETIMEDOUT)))
	require.False(t, connectionRefused(errors.New("connection refused")), "只认错误码,不认文案")
}

// startDroppingListener 起一个只建 TCP 连接、随即关掉的监听:每次取证新建的连接 gRPC 握手都失败,RPC 在期限之内回 Unavailable
// (修复前黑洞端点在缓存连接上从第二次取证起也是这样),原地址的 TCP 端口却没有拒连。
func startDroppingListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := listener.Addr().String()
	t.Cleanup(func() {
		discovery.RemoveEndpointConn(addr)
		_ = listener.Close()
	})
	return addr
}

// 评审 must_fix 的回归:原实例不在册、gRPC 查询每次都在期限之内就回错(修复前缓存连接停在 TRANSIENT_FAILURE 时就是这样),
// 修复前按「当场连不上 + 不在册」判原进程已不在、提前作废;现在要原地址的 TCP 端口当场拒连才算,连得上就判不了,连续几次取证都一样。
func TestDeadlineEvidenceProbeFastGrpcFailureIsNotRefusal(t *testing.T) {
	f := noRoomFormation(t, startDroppingListener(t), noRoomInstance, noRoomNowMs)
	dials := 0
	refuses := func(ctx context.Context, endpoint string) bool {
		dials++
		return originalAddressRefuses(ctx, endpoint)
	}
	probe := deadlineEvidenceProbe(askOriginalInstance, witnessOf(refuses, battleRegistryView{synced: true}))
	for attempt := 1; attempt <= 3; attempt++ {
		ctx := generousProbeContext(t)
		answer := askOriginalInstance(ctx, f)
		require.Error(t, answer.queryErr, "第 %d 次:gRPC 查询没成", attempt)
		require.NoError(t, ctx.Err(), "第 %d 次:在期限之内就失败了(修复前据此判原地址当场连不上)", attempt)
		require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(ctx, f), "第 %d 次:原地址连得上,不算拒连", attempt)
		require.Equal(t, attempt, dials, "第 %d 次:查询没成、原实例不在册时另拨了一次 TCP", attempt)
	}
}

// 评审补(2026-11-10)的回归:取证单独建连、用完即关,不在 discovery 的进程级缓存里留下到原地址的连接。修复前经 DialEndpoint
// 取证,原实例已摘册时缓存里留下一条停在 TRANSIENT_FAILURE、退避一路涨的连接,同一 ip:port 上来的新 battle 被撮合与观战拿到
// 这条旧连接、当场 Unavailable;原实例还活着时留下的是一条 READY 的连接。现在两种情形下取证之后,DialEndpoint 拿到的都是刚新建、
// 还没连过的连接(IDLE)。只看连接状态,不依赖墙钟;各端点的清理(RemoveEndpointConn)由夹具注册。
func TestAskOriginalInstanceLeavesNoCachedConnection(t *testing.T) {
	_, alive := startNoRoomBattle(t, &noRoomCalls{})
	for _, c := range []struct {
		why      string
		endpoint string
	}{
		{"原地址端口上没人听", closedLoopbackAddr(t)},
		{"原实例还在", alive},
	} {
		askOriginalInstance(generousProbeContext(t), noRoomFormation(t, c.endpoint, noRoomInstance, noRoomNowMs))
		conn, err := discovery.DialEndpoint(c.endpoint)
		require.NoError(t, err, c.why)
		require.Equal(t, connectivity.Idle, conn.GetState(), "%s:取证没有在进程级缓存里留下连接", c.why)
	}
}

// 原地址端口上没人听(进程不在、主机还在):镜像已同步且原实例已不在册才算原进程已不在;仍在册(停机排空时已停听、还没撤租约)、
// 镜像没同步过都判不了,且不拨。
func TestDeadlineEvidenceProbeCountsRefusalOnlyForAForgottenInstance(t *testing.T) {
	f := noRoomFormation(t, closedLoopbackAddr(t), noRoomInstance, noRoomNowMs)
	gone := deadlineEvidenceProbe(askOriginalInstance, witnessOf(originalAddressRefuses, battleRegistryView{synced: true}))
	require.Equal(t, authority.DeadlineEvidenceInstanceGone, gone(generousProbeContext(t), f))
	for why, view := range map[string]battleRegistryView{
		"仍在册":    {synced: true, listed: true},
		"镜像没同步过": {},
	} {
		probe := deadlineEvidenceProbe(askOriginalInstance, witnessOf(neverDial(t), view))
		require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(generousProbeContext(t), f), why)
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
	probe := deadlineEvidenceProbe(askOriginalInstance, witnessOf(neverDial(t), registry))

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

// 原地址上已不是 battle(方法当场回 Unimplemented):连得上就不算拒连,不论镜像怎么说都判不了(评审修正 2026-11-06,修复前
// 「当场失败 + 不在册」算原进程已不在)。期限已到(ctx 已取消)时同样判不了。
func TestDeadlineEvidenceProbeWhenTheEndpointIsNotABattle(t *testing.T) {
	f := noRoomFormation(t, startBareGRPC(t), noRoomInstance, noRoomNowMs)
	for why, view := range map[string]battleRegistryView{
		"仍在册":    {synced: true, listed: true},
		"镜像没同步过": {},
		"已不在册":   {synced: true},
	} {
		probe := deadlineEvidenceProbe(askOriginalInstance, witnessOf(originalAddressRefuses, view))
		require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(generousProbeContext(t), f), why)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	probe := deadlineEvidenceProbe(askOriginalInstance, witnessOf(originalAddressRefuses, battleRegistryView{synced: true}))
	require.Equal(t, authority.DeadlineEvidenceUndetermined, probe(expired, f), "期限已到:没问成,不算证据")
}

// 2026-11-11:实例级结论(提前作废与截止作废共用)。原实例亲口应答(不论这一局是什么状态)= 还活着;原地址上换了实例、
// 拒连且已不在册、K8s 里原 IP 上已没有存活的 Pod = 这一次观察说已不在;其余判不了。
func TestClassifyInstanceSightingTable(t *testing.T) {
	f := &pb.MatchFormationRecord{BattleId: 3201, OperationId: "deadline-op", TargetInstanceId: "original-instance"}
	echoed := func(state battlepb.BattleCreationState) originalInstanceAnswer {
		return originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "original-instance", State: state}, destroyErr: errTombstoneNotAttempted}
	}
	unavailable := errors.New("fixture: rpc error: code = Unavailable")
	failed := originalInstanceAnswer{destroyErr: unavailable, queryErr: unavailable}
	listed := battleRegistryView{synced: true, listed: true}
	gone := battleRegistryView{synced: true}
	for _, c := range []struct {
		why      string
		answer   originalInstanceAnswer
		refused  bool
		registry battleRegistryView
		podGone  bool
		want     instanceSighting
	}{
		{"回 PRESENT", echoed(battlepb.BattleCreationState_BATTLE_CREATION_PRESENT), false, listed, false, instanceAnswered},
		{"回 ABSENT(建房还在途)", echoed(battlepb.BattleCreationState_BATTLE_CREATION_ABSENT), false, listed, false, instanceAnswered},
		{"回 ABORTED", echoed(battlepb.BattleCreationState_BATTLE_CREATION_ABORTED), false, listed, false, instanceAnswered},
		{"回 UNKNOWN(记着别的 operation),但回显了自己", echoed(battlepb.BattleCreationState_BATTLE_CREATION_UNKNOWN), false, gone, true, instanceAnswered},
		{"原地址上应答的是另一个实例(空包)", originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{}}, false, listed, false, instanceSeenGone},
		{"回显了别的实例", originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{TargetInstanceId: "someone-else"}}, false, gone, true, instanceUndetermined},
		{"查询失败、拒连、已不在册", failed, true, gone, false, instanceSeenGone},
		{"查询失败、拒连、仍在册", failed, true, listed, false, instanceUndetermined},
		{"查询失败、没拒连、已不在册", failed, false, gone, false, instanceUndetermined},
		{"查询失败、原 IP 上已没有存活的 Pod", failed, false, listed, true, instanceSeenGone},
	} {
		require.Equal(t, c.want, classifyInstanceSighting(f, c.answer, c.refused, c.registry, c.podGone), c.why)
	}
	noInstance := &pb.MatchFormationRecord{BattleId: 3202, OperationId: "deadline-op"}
	require.Equal(t, instanceSeenGone, classifyInstanceSighting(noInstance, originalInstanceAnswer{query: &battlepb.QueryBattleCreationResponse{}}, false, listed, false),
		"成局没有实例号(坏数据)时空包照旧归成已不在;复核记录器不为空实例号记账,不会据它作废")
}

// 提前作废只向原实例做只读查询:不发 DestroyBattle,不给还没建成的原操作立拒收墓碑(建房的 Create 可能还在途,补签又是客户端
// 可以随时触发的)。原实例应答 = 还活着;原地址上换了实例 = 这一次观察说已不在。
func TestInstanceSightingProbeOnlyQueriesAndNeverTombstones(t *testing.T) {
	ctx := context.Background()
	probe := instanceSightingProbe(queryOriginalInstance, witnessOf(neverDial(t), battleRegistryView{synced: true, listed: true}))

	calls := &noRoomCalls{}
	creating, endpoint := startNoRoomBattle(t, calls)
	require.Equal(t, instanceAnswered, probe(ctx, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs)))
	require.Equal(t, []string{"QueryBattleCreation"}, calls.snapshot(), "只查询")
	require.False(t, creating.tombstoned(noRoomOperation), "没有房间的原操作不能被这次观察立墓碑")

	replacedCalls := &noRoomCalls{}
	_, replacedEndpoint := startNoRoomBattle(t, replacedCalls)
	require.Equal(t, instanceSeenGone, probe(ctx, noRoomFormation(t, replacedEndpoint, "replaced-instance", noRoomNowMs)))
	require.Equal(t, []string{"QueryBattleCreation"}, replacedCalls.snapshot())
}

// Pod 查询与原实例的交互并行,交互失败(这里是端口上没人听、原实例仍在册,拒连不算证据)时由它给出结论;
// 没装配 Pod 查询(podGone 为 nil)时同一个场景判不了。查询拿到的是成局的原执行地址。
func TestDeadlineEvidenceProbeCountsAGonePodWhenTheInstanceDoesNotAnswer(t *testing.T) {
	f := noRoomFormation(t, closedLoopbackAddr(t), noRoomInstance, noRoomNowMs)
	listed := battleRegistryView{synced: true, listed: true}
	var asked []string
	witness := witnessOf(neverDial(t), listed)
	witness.podGone = func(_ context.Context, endpoint string) bool {
		asked = append(asked, endpoint)
		return true
	}
	require.Equal(t, authority.DeadlineEvidenceInstanceGone, deadlineEvidenceProbe(askOriginalInstance, witness)(generousProbeContext(t), f))
	require.Equal(t, []string{f.TargetEndpoint}, asked)
	require.Equal(t, instanceSeenGone, instanceSightingProbe(queryOriginalInstance, witness)(generousProbeContext(t), f))

	witness.podGone = func(context.Context, string) bool { return false }
	require.Equal(t, authority.DeadlineEvidenceUndetermined, deadlineEvidenceProbe(askOriginalInstance, witness)(generousProbeContext(t), f), "原 IP 上还有存活的 Pod")
	require.Equal(t, authority.DeadlineEvidenceUndetermined, deadlineEvidenceProbe(askOriginalInstance, witnessOf(neverDial(t), listed))(generousProbeContext(t), f), "没装配 Pod 查询")
}

// 注册中心镜像:stable 与 canary 两份都查;没同步过不算同步(Upsert 只是测试灌节点);原 node_id 另登记了一个实例不单独算证据,
// 原实例仍在册就是在册。没装配镜像(单测)时不给任何证据。
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
	require.Equal(t, battleRegistryView{listed: true}, battleRegistryOf(s, f), "原 node_id 另登记了一个实例:不单独算证据,原实例仍在册")
}
