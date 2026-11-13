package clientplayerloginlogic

// EnterGame 提交会话时,记录在读与写之间被改过就重读重判(entergamelogic.go commitEnterGameSession)。
//
// 这是 gate 排空迁移与跨 zone 重定向的交错:客户端先关旧连接、再在新连接上 EnterGame,旧 gate 的断线上报落在
// 新登录链的 GetSession 与 SetSession 之间。旧代码按 ONLINE 定的顶号写入被 player_locator 拒绝后整条链失败,
// 给新连接推 3023,玩家在本该无感的迁移途中被打回登录界面(docs/design/gate-drain-session-migration.md §5)。
// 这里盯三件事:该重做的重做并改走短线重连;重做有上限;结果未知的错误不重做。
//
// 借用 entergame_kick_test.go 的 harness:Bind 因 gate_node_id = 0 被守卫拒掉,链在会话提交之后停下,碰不到 Kafka 与
// scene_manager。所以「提交成功」的用例里 apply 仍返回那条守卫错误,断言看的是 locator 收到的调用序列。

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"login/internal/logic/pkg/sessionmanager"
	plpb "proto/player_locator"
)

// redoCount 读 login_enter_session_commit_redo_total{decision}。指标是进程级全局量,用例一律比较前后差值。
func redoCount(t *testing.T, decision string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_enter_session_commit_redo_total", map[string]string{"decision": decision})
}

// locatorSessionChanged 是 player_locator 对「记录变了」的应答(setsessionlogic.go sessionChangedError)。
func locatorSessionChanged() error {
	return status.Error(codes.Aborted, "stale session update for player 7101: current version=5 incoming=5")
}

// 旧连接断线后的会话记录:DISCONNECTING,版本比 ONLINE 那条大一格(SetDisconnecting 推进版本)。
func kickDisconnectingRecord(online *plpb.PlayerSession) *plpb.PlayerSession {
	record := proto.Clone(online).(*plpb.PlayerSession)
	record.State = plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	record.SessionVersion = online.GetSessionVersion() + 1
	return record
}

// 第一遍读到 ONLINE、按顶号写入被拒;重读看到 DISCONNECTING,改走短线重连并提交。不踢线、不复读消歧、不挂租约。
func TestEnterGame_ReplaceLoginInterruptedByDisconnectRedoesAsShortReconnect(t *testing.T) {
	online := kickExistingRecord(kickOldSessionID, kickOldInstance)
	h := newKickHarness(t, online, kickChainInstance)
	h.locator.getSessionReplies = []kickGetSessionReply{{session: online}, {session: kickDisconnectingRecord(online)}}
	h.locator.setSessionErrs = []error{locatorSessionChanged()}
	redoBefore := redoCount(t, "replace")
	uncommittedBefore := kickCount(t, kickResultSkipUncommitted)

	err := h.apply(context.Background())

	// 会话已按短线重连提交;随后本链的 Bind 因 gate_node_id = 0 被守卫拒掉(harness 的停点)。
	if err == nil || !strings.Contains(err.Error(), "gate node id is 0") {
		t.Fatalf("apply error = %v, want the chain to commit the session and stop at the Bind node-id guard", err)
	}
	if got, want := h.events.snapshot(), []string{"GetSession", "SetSession", "GetSession", "Reconnect"}; !slices.Equal(got, want) {
		t.Fatalf("event sequence = %v, want %v (re-read and re-decide after the rejected write, no outcome probe, no kick)", got, want)
	}
	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none: the old connection is already disconnecting", calls)
	}

	h.locator.mu.Lock()
	rejected := h.locator.setSessions[0].GetSession()
	reconnect := h.locator.reconnects[0]
	leases := len(h.locator.disconnecting)
	h.locator.mu.Unlock()
	if reconnect.GetPlayerId() != kickPlayerID || reconnect.GetNewSessionId() != kickChainSessionID ||
		reconnect.GetGateInstanceId() != kickChainInstance || reconnect.GetAccount() != kickAccount {
		t.Fatalf("reconnect request = %+v, want this chain's player, session, gate instance and account", reconnect)
	}
	// 每一遍各铸一次代次:重做用的必须比被拒那一遍的大,也比记录里的大(locator 要求严格递增)。
	if reconnect.GetLoginGeneration() <= rejected.GetLoginGeneration() || reconnect.GetLoginGeneration() <= kickRecordGeneration {
		t.Fatalf("reconnect login_generation = %d, want greater than the rejected attempt's %d and the record's %d",
			reconnect.GetLoginGeneration(), rejected.GetLoginGeneration(), kickRecordGeneration)
	}
	if leases != 0 {
		t.Fatalf("SetDisconnecting calls = %d, want 0: nothing was committed by the rejected attempt, no lease to place", leases)
	}
	if got := redoCount(t, "replace") - redoBefore; got != 1 {
		t.Fatalf("login_enter_session_commit_redo_total{decision=\"replace\"} delta = %v, want 1", got)
	}
	requireKickDelta(t, kickResultSkipUncommitted, uncommittedBefore, 0)
}

// 每一遍都被拒:做满 enterGameSessionCommitAttempts 遍后按链失败收尾,错误仍带 ErrSessionChanged;全程不踢线。
func TestEnterGame_SessionChangedOnEveryAttemptFailsAfterBoundedRedos(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.locator.setSessionErr = locatorSessionChanged()
	redoBefore := redoCount(t, "replace")

	err := h.apply(context.Background())

	if !errors.Is(err, sessionmanager.ErrSessionChanged) {
		t.Fatalf("apply error = %v, want it to carry ErrSessionChanged after the last attempt", err)
	}
	h.locator.mu.Lock()
	reads, writes := h.locator.getSessionCalls, len(h.locator.setSessions)
	generations := make([]uint64, 0, writes)
	for _, req := range h.locator.setSessions {
		generations = append(generations, req.GetSession().GetLoginGeneration())
	}
	h.locator.mu.Unlock()
	if reads != enterGameSessionCommitAttempts || writes != enterGameSessionCommitAttempts {
		t.Fatalf("GetSession calls = %d, SetSession calls = %d, want %d each (one read per write, no outcome probe)",
			reads, writes, enterGameSessionCommitAttempts)
	}
	for i := 1; i < len(generations); i++ {
		if generations[i] <= generations[i-1] {
			t.Fatalf("login generations across attempts = %v, want strictly increasing (each attempt mints its own)", generations)
		}
	}
	if calls := h.kicker.recorded(); len(calls) != 0 {
		t.Fatalf("kicks = %+v, want none: no attempt committed", calls)
	}
	if got, want := redoCount(t, "replace")-redoBefore, float64(enterGameSessionCommitAttempts-1); got != want {
		t.Fatalf("login_enter_session_commit_redo_total{decision=\"replace\"} delta = %v, want %v", got, want)
	}
}

// 结果未知的 SetSession 错误(超时、连接断开)不重做:只写一次,照旧复读一次消歧。
func TestEnterGame_UnknownSetSessionErrorIsNotRedone(t *testing.T) {
	h := newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	h.locator.setSessionErr = errKickSetSessionUnavailable
	redoBefore := redoCount(t, "replace")

	err := h.apply(context.Background())

	if !errors.Is(err, errKickSetSessionUnavailable) || errors.Is(err, sessionmanager.ErrSessionChanged) {
		t.Fatalf("apply error = %v, want the original SetSession error without ErrSessionChanged", err)
	}
	if got, want := h.events.snapshot(), []string{"GetSession", "SetSession", "GetSession"}; !slices.Equal(got, want) {
		t.Fatalf("event sequence = %v, want %v (one write, then the outcome probe)", got, want)
	}
	if got := redoCount(t, "replace") - redoBefore; got != 0 {
		t.Fatalf("login_enter_session_commit_redo_total{decision=\"replace\"} delta = %v, want 0", got)
	}
}
