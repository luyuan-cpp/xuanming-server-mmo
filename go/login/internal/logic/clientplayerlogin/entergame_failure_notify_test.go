package clientplayerloginlogic

import (
	"errors"
	"testing"

	pbbase "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"
)

// EnterGame 异步链的失败出口(notifyEnterGameFailed)的单测。
//
// 契约:EnterGame 的 gRPC 应答无错 = 已受理;之后进场没成,服务端经 gate 推 SendTipToClient,
// tip = kEnterSceneFailed。命令本身的三层信封与寻址由 svc/gate_command_test.go 盯住,
// 这里只盯"该不该推、推给谁、推失败怎么办"。

const failureNotifyMetric = "entergame_failure_notify_total"

// recordingTipPusher 是 enterFailureTipPusher 的假实现:记下每一次调用的实参,按 err 应答。
type recordingTipPusher struct {
	err   error
	calls []recordedTipPush
}

type recordedTipPush struct {
	gateID         string
	gateInstanceID string
	sessionID      uint32
	playerID       uint64
	tipID          uint32
}

func (p *recordingTipPusher) PushTipToSession(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, tipID uint32) error {
	p.calls = append(p.calls, recordedTipPush{gateID, gateInstanceID, sessionID, playerID, tipID})
	return p.err
}

// failureNotifyCount 读 failure_notify_total 某条序列的当前值;计数是进程级累加的,用例只比增量。
// 沿用 createPlayerCounterValue:它会先打开 go-zero 的 prometheus 全局开关再 gather。
func failureNotifyCount(t *testing.T, stage, outcome string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, failureNotifyMetric, map[string]string{"stage": stage, "outcome": outcome})
}

func gateAddressedState() enterGameSessionState {
	return enterGameSessionState{
		playerID:       identityPlayerID,
		sessionID:      enterWiringSessionID,
		gateID:         "300",
		gateInstanceID: "gate-b-uuid",
	}
}

// 有寻址信息:推一次,目标是**本连接**的会话,tip 固定为 kEnterSceneFailed(不随失败阶段变)。
func TestNotifyEnterGameFailed_PushesEnterSceneFailedToOwnSession(t *testing.T) {
	for _, stage := range []string{notifyStagePreload, notifyStageApply} {
		t.Run(stage, func(t *testing.T) {
			before := failureNotifyCount(t, stage, notifyOutcomeSent)
			pusher := &recordingTipPusher{}

			notifyEnterGameFailed(pusher, gateAddressedState(), stage)

			if len(pusher.calls) != 1 {
				t.Fatalf("PushTipToSession calls = %d, want exactly 1 (no retry, no fan-out)", len(pusher.calls))
			}
			want := recordedTipPush{
				gateID:         "300",
				gateInstanceID: "gate-b-uuid",
				sessionID:      enterWiringSessionID,
				playerID:       identityPlayerID,
				tipID:          uint32(table.SceneError_kEnterSceneFailed),
			}
			if pusher.calls[0] != want {
				t.Fatalf("push = %+v, want %+v", pusher.calls[0], want)
			}
			if got := failureNotifyCount(t, stage, notifyOutcomeSent) - before; got != 1 {
				t.Fatalf("failure_notify_total{stage=%s,outcome=sent} delta = %v, want 1", stage, got)
			}
		})
	}
}

// 推送失败:只记数,不重试、不 panic、不向上传播(函数没有返回值,这里盯的是"只调一次")。
func TestNotifyEnterGameFailed_PushErrorIsCountedNotRetried(t *testing.T) {
	beforeFailed := failureNotifyCount(t, notifyStageApply, notifyOutcomeFailed)
	beforeSent := failureNotifyCount(t, notifyStageApply, notifyOutcomeSent)
	pusher := &recordingTipPusher{err: errors.New("kafka unavailable")}

	notifyEnterGameFailed(pusher, gateAddressedState(), notifyStageApply)

	if len(pusher.calls) != 1 {
		t.Fatalf("PushTipToSession calls = %d, want 1 (a push to a possibly-gone session must not be retried)", len(pusher.calls))
	}
	if got := failureNotifyCount(t, notifyStageApply, notifyOutcomeFailed) - beforeFailed; got != 1 {
		t.Fatalf("outcome=failed delta = %v, want 1", got)
	}
	if got := failureNotifyCount(t, notifyStageApply, notifyOutcomeSent) - beforeSent; got != 0 {
		t.Fatalf("outcome=sent delta = %v, want 0", got)
	}
}

// 会话上没有 gate 寻址信息(dev 旁路 / 旧版 gate / gate_node_id 没填):不推,不硬造寻址。
func TestNotifyEnterGameFailed_SkipsWithoutGateAddress(t *testing.T) {
	cases := map[string]func(*enterGameSessionState){
		"empty gate instance id": func(s *enterGameSessionState) { s.gateInstanceID = "" },
		// buildEnterGameSessionState 把没填的 gate_node_id 格式化成 "0"。
		"unset gate node id": func(s *enterGameSessionState) { s.gateID = "0" },
		"empty gate id":      func(s *enterGameSessionState) { s.gateID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			before := failureNotifyCount(t, notifyStagePreload, notifyOutcomeSkippedNoGate)
			state := gateAddressedState()
			mutate(&state)
			pusher := &recordingTipPusher{}

			notifyEnterGameFailed(pusher, state, notifyStagePreload)

			if len(pusher.calls) != 0 {
				t.Fatalf("PushTipToSession calls = %d, want 0", len(pusher.calls))
			}
			if got := failureNotifyCount(t, notifyStagePreload, notifyOutcomeSkippedNoGate) - before; got != 1 {
				t.Fatalf("outcome=skipped_no_gate delta = %v, want 1", got)
			}
		})
	}
}

// 接线:从 EnterGame 入口打进去,apply 失败的分支必须真的走到失败出口。
//
// 沿用 newEnterWiringHarness:链在会话落盘(SetSession 失败)处收尾,即 applyErr != nil。
// 该 harness 的 SessionDetails 不带 gate 寻址,所以出口落在 skipped_no_gate —— 这里要证明的是
// "失败分支调用了出口",不是推送本身(ServiceContext.KafkaClient 是具体类型,单测里起不出来;
// 推送路径由上面几条用例经 enterFailureTipPusher 覆盖)。
// 删掉 onPreloadComplete 里的 `failedStage = notifyStageApply`,本用例变红。
func TestEnterGame_ApplyFailureReachesFailureExit(t *testing.T) {
	// 账号记录里直接有这个角色(带名字):归属校验通过,且不回源名字注册表。
	h := newEnterWiringHarness(t,
		[]*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}, nil)
	before := failureNotifyCount(t, notifyStageApply, notifyOutcomeSkippedNoGate)
	beforePreload := failureNotifyCount(t, notifyStagePreload, notifyOutcomeSkippedNoGate)

	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
	if err != nil {
		t.Fatalf("EnterGame: %v", err)
	}
	if resp.GetErrorMessage() != nil {
		t.Fatalf("EnterGame rejected with tip %d, want the async chain started", resp.GetErrorMessage().GetId())
	}
	h.waitChain(t)

	if got := h.locator.setSessionCalls.Load(); got != 1 {
		t.Fatalf("SetSession calls = %d, want 1 (chain must fail at persist, i.e. inside apply)", got)
	}
	if got := failureNotifyCount(t, notifyStageApply, notifyOutcomeSkippedNoGate) - before; got != 1 {
		t.Fatalf("failure_notify_total{stage=apply,outcome=skipped_no_gate} delta = %v, want 1 (apply failure must reach the failure exit)", got)
	}
	if got := failureNotifyCount(t, notifyStagePreload, notifyOutcomeSkippedNoGate) - beforePreload; got != 0 {
		t.Fatalf("stage=preload delta = %v, want 0 (preload succeeded on the fast path)", got)
	}
}
