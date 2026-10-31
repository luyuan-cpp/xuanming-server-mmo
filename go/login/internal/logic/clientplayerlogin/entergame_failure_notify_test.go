package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	pbbase "proto/common/base"
	loginpb "proto/login"
	smpb "proto/scene_manager"
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
	unbound        bool // 走的是 PushTipToUnboundSession
}

func (p *recordingTipPusher) PushTipToSession(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, tipID uint32) error {
	p.calls = append(p.calls, recordedTipPush{gateID, gateInstanceID, sessionID, playerID, tipID, false})
	return p.err
}

func (p *recordingTipPusher) PushTipToUnboundSession(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, tipID uint32) error {
	p.calls = append(p.calls, recordedTipPush{gateID, gateInstanceID, sessionID, playerID, tipID, true})
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
// Bind 之前走未绑定的发送(gate 上的会话还没有绑定本玩家,带真实 player_id 必被身份栅栏丢弃),Bind 之后走带栅栏的发送。
func TestNotifyEnterGameFailed_PushesEnterSceneFailedToOwnSession(t *testing.T) {
	for _, stage := range []string{notifyStageApply} {
		for _, bindSent := range []bool{false, true} {
			name := stage + "/before_bind"
			if bindSent {
				name = stage + "/after_bind"
			}
			t.Run(name, func(t *testing.T) {
				before := failureNotifyCount(t, stage, notifyOutcomeSent)
				pusher := &recordingTipPusher{}

				notifyEnterGameFailed(pusher, gateAddressedState(), stage, bindSent)

				if len(pusher.calls) != 1 {
					t.Fatalf("push calls = %d, want exactly 1 (no retry, no fan-out)", len(pusher.calls))
				}
				want := recordedTipPush{
					gateID:         "300",
					gateInstanceID: "gate-b-uuid",
					sessionID:      enterWiringSessionID,
					playerID:       identityPlayerID,
					tipID:          uint32(table.SceneError_kEnterSceneFailed),
					unbound:        !bindSent,
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
}

// bindRecorder 是 sessionBindSender 的假实现:记调用次数,按 err 应答,不碰 Kafka。
type bindRecorder struct {
	err   error
	calls int
}

func (b *bindRecorder) SendBindSessionToGate(string, string, uint32, uint64, uint32, uint32, uint64) error {
	b.calls++
	return b.err
}

// failingSceneManager 让 Bind 之后的 EnterScene 失败,链在 Bind 已发出的状态下收尾。
type failingSceneManager struct {
	smpb.SceneManagerClient
	calls int
}

func (f *failingSceneManager) EnterScene(context.Context, *smpb.EnterSceneRequest, ...grpc.CallOption) (*smpb.EnterSceneResponse, error) {
	f.calls++
	return nil, errors.New("scene_manager unavailable")
}

// 链上哪一步失败决定 bindSent:只有 Bind 确实写进 Kafka 之后才为 true。Bind 写失败时结果未知,按没发出处理:
// player_id = 0 在会话绑定与否两种情况下都能送达。
func TestApplyLoadedPlayerSession_RecordsWhetherBindWasSent(t *testing.T) {
	newHarness := func(t *testing.T) *kickHarness {
		return newKickHarness(t, kickExistingRecord(kickOldSessionID, kickOldInstance), kickChainInstance)
	}
	t.Run("session write fails before bind", func(t *testing.T) {
		h := newHarness(t)
		h.locator.setSessionErr = errKickSetSessionUnavailable
		binder := &bindRecorder{}
		h.l.binder = binder

		if err := h.apply(context.Background()); err == nil {
			t.Fatal("apply: want the SetSession error")
		}
		if binder.calls != 0 || h.bindSent {
			t.Fatalf("bind calls = %d, bindSent = %t; want no bind", binder.calls, h.bindSent)
		}
	})
	t.Run("bind send fails", func(t *testing.T) {
		h := newHarness(t)
		binder := &bindRecorder{err: errors.New("kafka unavailable")}
		h.l.binder = binder

		if err := h.apply(context.Background()); err == nil {
			t.Fatal("apply: want the bind error")
		}
		if binder.calls != 1 || h.bindSent {
			t.Fatalf("bind calls = %d, bindSent = %t; want one attempt, not counted as sent", binder.calls, h.bindSent)
		}
	})
	t.Run("enter scene fails after bind", func(t *testing.T) {
		h := newHarness(t)
		binder := &bindRecorder{}
		sceneManager := &failingSceneManager{}
		h.l.binder = binder
		h.l.svcCtx.SceneManagerClient = sceneManager

		if err := h.apply(context.Background()); err == nil {
			t.Fatal("apply: want the EnterScene error")
		}
		if binder.calls != 1 || sceneManager.calls != 1 || !h.bindSent {
			t.Fatalf("bind calls = %d, EnterScene calls = %d, bindSent = %t; want the chain to fail after a sent bind",
				binder.calls, sceneManager.calls, h.bindSent)
		}
	})
}

// 推送失败:只记数,不重试、不 panic、不向上传播(函数没有返回值,这里盯的是"只调一次")。
func TestNotifyEnterGameFailed_PushErrorIsCountedNotRetried(t *testing.T) {
	beforeFailed := failureNotifyCount(t, notifyStageApply, notifyOutcomeFailed)
	beforeSent := failureNotifyCount(t, notifyStageApply, notifyOutcomeSent)
	pusher := &recordingTipPusher{err: errors.New("kafka unavailable")}

	notifyEnterGameFailed(pusher, gateAddressedState(), notifyStageApply, true)

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
			before := failureNotifyCount(t, notifyStageApply, notifyOutcomeSkippedNoGate)
			state := gateAddressedState()
			mutate(&state)
			pusher := &recordingTipPusher{}

			notifyEnterGameFailed(pusher, state, notifyStageApply, false)

			if len(pusher.calls) != 0 {
				t.Fatalf("PushTipToSession calls = %d, want 0", len(pusher.calls))
			}
			if got := failureNotifyCount(t, notifyStageApply, notifyOutcomeSkippedNoGate) - before; got != 1 {
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
}
