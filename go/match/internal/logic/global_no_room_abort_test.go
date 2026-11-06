package logic

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"match/internal/authority"
	"match/internal/discovery"
	battlepb "proto/battle"
	base "proto/common/base"
	pb "proto/match"
)

// 2026-11-05:提前 ABORT(global_no_room_abort.go)的 logic 层回归。回环 gRPC 上起一个桩 battle 节点,按真实 battle 的语义
// 回 DestroyBattle(尚无房间的原操作立拒收墓碑,有房间忽略,实例不符拒绝)与 QueryBattleCreation(实例不符回空包);
// 替身权威记下调用。不起 TiDB、不依赖墙钟:SQL 时刻由替身给定。锁内复核与真库写入在 authority/terminal_abort_test.go。

// noRoomCalls 按发生顺序记下替身权威与桩 battle 收到的调用,用例据此断言「先读 SQL 时刻,再立墓碑、查询,最后提交」。
type noRoomCalls struct {
	mu    sync.Mutex
	order []string
}

func (c *noRoomCalls) add(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order, call)
}

func (c *noRoomCalls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

type noRoomBattleNode struct {
	battlepb.UnimplementedBattleNodeServer
	calls    *noRoomCalls
	mu       sync.Mutex
	instance string
	rooms    map[string]bool // operation → 房间存在
	aborted  map[string]bool // operation → 拒收墓碑
}

func (b *noRoomBattleNode) DestroyBattle(_ context.Context, req *battlepb.DestroyBattleRequest) (*base.Empty, error) {
	b.calls.add("DestroyBattle")
	b.mu.Lock()
	defer b.mu.Unlock()
	if req.GetTargetInstanceId() != b.instance {
		return nil, status.Error(codes.FailedPrecondition, "placement_operation_mismatch")
	}
	if !b.rooms[req.GetPlacementOperationId()] {
		b.aborted[req.GetPlacementOperationId()] = true
	}
	return &base.Empty{}, nil
}

func (b *noRoomBattleNode) QueryBattleCreation(_ context.Context, req *battlepb.QueryBattleCreationRequest) (*battlepb.QueryBattleCreationResponse, error) {
	b.calls.add("QueryBattleCreation")
	b.mu.Lock()
	defer b.mu.Unlock()
	if req.GetTargetInstanceId() != b.instance {
		return &battlepb.QueryBattleCreationResponse{}, nil
	}
	resp := &battlepb.QueryBattleCreationResponse{TargetInstanceId: b.instance, SupportedOutcomeProtocolVersion: 1, State: battlepb.BattleCreationState_BATTLE_CREATION_ABSENT}
	switch op := req.GetPlacementOperationId(); {
	case b.aborted[op]:
		resp.State = battlepb.BattleCreationState_BATTLE_CREATION_ABORTED
	case b.rooms[op]:
		resp.State = battlepb.BattleCreationState_BATTLE_CREATION_PRESENT
		resp.AdoptedOutcomeProtocolVersion = 1
	}
	return resp, nil
}

func (b *noRoomBattleNode) tombstoned(operation string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.aborted[operation]
}

type noRoomAuthorityFake struct {
	calls     *noRoomCalls
	nowMs     uint64
	nowErr    error
	noIntent  bool  // 本代租约下没有 Create 原件
	intentErr error // CreateIntentPersisted 的错误(租约被接手、读库失败)
	abortErr  error
	proofs    []authority.NoRoomProof
}

func (f *noRoomAuthorityFake) CreateIntentPersisted(context.Context, *pb.MatchFormationRecord) (bool, error) {
	f.calls.add("CreateIntentPersisted")
	if f.intentErr != nil {
		return false, f.intentErr
	}
	return !f.noIntent, nil
}

func (f *noRoomAuthorityFake) RecordBattleAdopted(_ context.Context, _ *pb.MatchFormationRecord, _ string, version uint32) error {
	f.calls.add(fmt.Sprintf("RecordBattleAdopted(%d)", version))
	return nil
}

func (f *noRoomAuthorityFake) Finish(_ context.Context, _ *pb.MatchFormationRecord, success bool) error {
	f.calls.add(fmt.Sprintf("Finish(%v)", success))
	return nil
}

func (f *noRoomAuthorityFake) Now(context.Context) (uint64, error) {
	f.calls.add("Now")
	return f.nowMs, f.nowErr
}

func (f *noRoomAuthorityFake) AbortWithoutRoom(_ context.Context, _ *pb.MatchFormationRecord, proof authority.NoRoomProof) (pb.BattleOutcomeDecision, error) {
	f.calls.add("AbortWithoutRoom")
	f.proofs = append(f.proofs, proof)
	if f.abortErr != nil {
		return 0, f.abortErr
	}
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}

const (
	noRoomNowMs     uint64 = 1_800_000_000_000
	noRoomOperation        = "original-op"
	noRoomInstance         = "original-instance"
)

// startNoRoomBattle 起桩 battle 节点,返回节点与端点;用例结束时关掉并清掉连接缓存。
func startNoRoomBattle(t *testing.T, calls *noRoomCalls) (*noRoomBattleNode, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	battle := &noRoomBattleNode{calls: calls, instance: noRoomInstance, rooms: map[string]bool{}, aborted: map[string]bool{}}
	server := grpc.NewServer()
	battlepb.RegisterBattleNodeServer(server, battle)
	go func() { _ = server.Serve(listener) }()
	addr := listener.Addr().String()
	t.Cleanup(func() {
		discovery.RemoveEndpointConn(addr)
		server.Stop()
	})
	return battle, addr
}

// noRoomFormation 是一局已持久 v1 Create 原件、还没建成(state 2)的成局;原件准入期限为 admitBeforeMs。
func noRoomFormation(t *testing.T, endpoint, instance string, admitBeforeMs uint64) *pb.MatchFormationRecord {
	t.Helper()
	request := &battlepb.CreateBattleRequest{BattleId: 3101, PlacementOperationId: noRoomOperation, TargetInstanceId: instance, OutcomeProtocolVersion: 1, AdmitBeforeMs: admitBeforeMs, DeadlineMs: admitBeforeMs + 600_000}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	return &pb.MatchFormationRecord{BattleId: 3101, OperationId: noRoomOperation, State: 2, TargetInstanceId: instance, TargetEndpoint: endpoint, OutcomeProtocolVersion: 1, CreateRequest: raw, RetryAtMs: 42}
}

// 准入期限刚过、还没过时钟余量:battle 主机时钟稍慢时仍可能接这份 Create。不碰 battle,不提交,照旧等截止。
func TestNoRoomAbortWaitsUntilAdmissionClosed(t *testing.T) {
	calls := &noRoomCalls{}
	_, endpoint := startNoRoomBattle(t, calls)
	store := &noRoomAuthorityFake{calls: calls, nowMs: noRoomNowMs}
	decision, err := abortProvablyNoRoom(store, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-1))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Equal(t, []string{"Now"}, calls.snapshot(), "不向 battle 发任何 RPC")
}

// 准入已关、原实例没有这一局的房间:先读 SQL 时刻,再立拒收墓碑、查到 ABORTED,最后带着「读时刻在前」的证据提交。
func TestNoRoomAbortTombstonesTargetThenCommitsWithProof(t *testing.T) {
	calls := &noRoomCalls{}
	battle, endpoint := startNoRoomBattle(t, calls)
	store := &noRoomAuthorityFake{calls: calls, nowMs: noRoomNowMs}
	decision, err := abortProvablyNoRoom(store, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	require.Equal(t, []string{"Now", "CreateIntentPersisted", "DestroyBattle", "QueryBattleCreation", "AbortWithoutRoom"}, calls.snapshot(),
		"SQL 时刻必须在立墓碑与查询之前读到")
	require.Equal(t, []authority.NoRoomProof{{OperationId: noRoomOperation, TargetInstanceId: noRoomInstance, ObservedAfterMs: noRoomNowMs}}, store.proofs)
	require.True(t, battle.tombstoned(noRoomOperation), "原实例对原操作留了拒收墓碑")
}

// 原实例上房间还在:DestroyBattle 被忽略、查询回 PRESENT,按既有规则记采用版本并提交 state 4,绝不提前 ABORT。
func TestNoRoomAbortNeverAbortsWhenRoomPresent(t *testing.T) {
	calls := &noRoomCalls{}
	battle, endpoint := startNoRoomBattle(t, calls)
	battle.rooms[noRoomOperation] = true
	store := &noRoomAuthorityFake{calls: calls, nowMs: noRoomNowMs}
	decision, err := abortProvablyNoRoom(store, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Empty(t, store.proofs)
	require.NotContains(t, calls.snapshot(), "AbortWithoutRoom")
	require.Contains(t, calls.snapshot(), "Finish(true)")
	require.False(t, battle.tombstoned(noRoomOperation))
}

// 端点上已换成另一个进程(UUID 不符):DestroyBattle 被拒、查询回空包,拿不到证据,照旧等截止。
func TestNoRoomAbortNeedsTheOriginalInstance(t *testing.T) {
	calls := &noRoomCalls{}
	_, endpoint := startNoRoomBattle(t, calls)
	store := &noRoomAuthorityFake{calls: calls, nowMs: noRoomNowMs}
	decision, err := abortProvablyNoRoom(store, noRoomFormation(t, endpoint, "replaced-instance", noRoomNowMs-10*60_000))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Empty(t, store.proofs)
}

// 读 SQL 时刻失败什么都不判;提交时租约已被别的副本接手不算本副本失败(交给接手者),其余提交错误原样上抛。
func TestNoRoomAbortClockAndLeaseFailures(t *testing.T) {
	calls := &noRoomCalls{}
	_, endpoint := startNoRoomBattle(t, calls)
	broken := errors.New("fixture: SQL 时刻读不到")
	store := &noRoomAuthorityFake{calls: calls, nowErr: broken}
	_, err := abortProvablyNoRoom(store, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
	require.ErrorIs(t, err, broken)
	require.Equal(t, []string{"Now"}, calls.snapshot())

	lost := &noRoomAuthorityFake{calls: &noRoomCalls{}, nowMs: noRoomNowMs, abortErr: authority.ErrWorkLease}
	decision, err := abortProvablyNoRoom(lost, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)

	unknown := errors.New("fixture: 提交结果未知")
	failing := &noRoomAuthorityFake{calls: &noRoomCalls{}, nowMs: noRoomNowMs, abortErr: unknown}
	_, err = abortProvablyNoRoom(failing, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
	require.ErrorIs(t, err, unknown)

	missing := noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000)
	missing.CreateRequest = nil
	decision, err = abortProvablyNoRoom(&noRoomAuthorityFake{calls: &noRoomCalls{}, nowMs: noRoomNowMs}, missing)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision, "没有原件不归这条路径")
}

// 提前 ABORT 的证据只能是「原实例立了墓碑并回 ABORTED」,不能借 globalDestroy「本代租约下无原件」那条不问 battle 的 true
// (2026-11-05 评审修正)。本代租约下查不到原件、租约已被接手、读库失败,都不碰 battle、不提交;改回经 globalDestroy 取证据,
// 第一条会直接走到 AbortWithoutRoom、在没有墓碑的情况下提前 ABORT,用例转红。
func TestNoRoomAbortNeverTreatsMissingIntentAsTombstone(t *testing.T) {
	broken := errors.New("fixture: 原件读不到")
	for _, c := range []struct {
		why     string
		fake    noRoomAuthorityFake
		wantErr error
	}{
		{why: "本代租约下没有原件", fake: noRoomAuthorityFake{noIntent: true}},
		{why: "租约已被别的副本接手", fake: noRoomAuthorityFake{intentErr: authority.ErrWorkLease}},
		{why: "读库失败", fake: noRoomAuthorityFake{intentErr: broken}, wantErr: broken},
	} {
		calls := &noRoomCalls{}
		battle, endpoint := startNoRoomBattle(t, calls)
		store := c.fake
		store.calls, store.nowMs = calls, noRoomNowMs
		decision, err := abortProvablyNoRoom(&store, noRoomFormation(t, endpoint, noRoomInstance, noRoomNowMs-10*60_000))
		if c.wantErr != nil {
			require.ErrorIs(t, err, c.wantErr, c.why)
		} else {
			require.NoError(t, err, c.why)
			require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision, c.why)
		}
		require.Equal(t, []string{"Now", "CreateIntentPersisted"}, calls.snapshot(), "%s:不碰 battle、不提交", c.why)
		require.Empty(t, store.proofs, c.why)
		require.False(t, battle.tombstoned(noRoomOperation), c.why)
	}
}

// globalAbortPrepared 有原件的分支先争提前 ABORT,判不了再走截止恢复。globalAbortPrepared 依赖具体的 *authority.Store
// 与真 TiDB,这里按源码结构机械核对接线(同 global_outcome_recover_test.go 的做法):删掉这次调用,或挪到截止恢复之后、
// 挪出有原件的分支,用例转红。
func TestGlobalAbortPreparedTriesNoRoomAbortBeforeDeadlineRecovery(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "global_match.go", nil, 0)
	require.NoError(t, err, "解析 global_match.go 失败;文件改名就同步改这里")
	var prepared *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "globalAbortPrepared" {
			prepared = fn
		}
	}
	require.NotNil(t, prepared, "global_match.go 里找不到 globalAbortPrepared;改名就同步改这里")
	var withOriginal *ast.BlockStmt
	for _, stmt := range prepared.Body.List {
		branch, ok := stmt.(*ast.IfStmt)
		if !ok {
			continue
		}
		cond, ok := branch.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.NEQ {
			continue
		}
		if call, ok := cond.X.(*ast.CallExpr); ok {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "len" {
				withOriginal = branch.Body
			}
		}
	}
	require.NotNil(t, withOriginal, "globalAbortPrepared 里找不到 `if len(f.CreateRequest) != 0` 分支;改了写法就同步改这里")
	var noRoom, deadline token.Pos
	ast.Inspect(withOriginal, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "abortProvablyNoRoom" && noRoom == token.NoPos {
				noRoom = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "RecoverExpiredOutcome" && deadline == token.NoPos {
				deadline = call.Pos()
			}
		}
		return true
	})
	require.NotEqual(t, token.NoPos, noRoom, "有原件的分支不再调 abortProvablyNoRoom:能证明不会建房的局又要等到截止")
	require.NotEqual(t, token.NoPos, deadline, "有原件的分支不再调 RecoverExpiredOutcome:截止后的局没有出口")
	require.Less(t, int(noRoom), int(deadline), "提前 ABORT 要先于截止恢复")
}
