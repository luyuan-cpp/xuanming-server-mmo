package svc

import (
	"context"
	"sync"
	"testing"

	"chat/internal/authority"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"
	"shared/drainproof"

	"github.com/stretchr/testify/require"
)

// recheckStore 是补审循环用到的那几个库操作的替身;其余方法不该被调到(内嵌的 nil 接口一调就 panic)。
type recheckStore struct {
	authority.Service
	mu        sync.Mutex
	tasks     []authority.PlatformRecheckTask
	finished  map[int64]authority.RecheckOutcome
	retracts  int
	retractOK bool
}

func (s *recheckStore) ClaimPlatformRechecks(context.Context, int) ([]authority.PlatformRecheckTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.tasks
	s.tasks = nil
	return tasks, nil
}

func (s *recheckStore) FinishPlatformRecheck(_ context.Context, _ uint64, acceptedMs int64, outcome authority.RecheckOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished[acceptedMs] = outcome
	return nil
}

func (s *recheckStore) RetractWorld(context.Context, uint64, int64, string) (bool, uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retracts++
	return s.retractOK, 0, nil
}

type retractRecorder struct {
	mu        sync.Mutex
	retracted []*chatpb.ChatRetractedNotify
}

func (r *retractRecorder) PushWorld(context.Context, *chatpb.ChatMessage) error { return nil }
func (r *retractRecorder) PushWorldRetract(_ context.Context, notify *chatpb.ChatRetractedNotify) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retracted = append(r.retracted, notify)
	return nil
}

// 一批待办逐条判:违规的撤回并推送,通过的记通过,平台不可用的不记结局(租期过后再试),
// 用完次数的、原件不在的记放弃,补审前已撤回的不送平台。
func TestPlatformRecheckDecidesEachClaimedTask(t *testing.T) {
	store := &recheckStore{finished: map[int64]authority.RecheckOutcome{}, retractOK: true, tasks: []authority.PlatformRecheckTask{
		{SenderPlayerID: 1, AcceptedMs: 100, Content: "违规"},
		{SenderPlayerID: 2, AcceptedMs: 200, Content: "正常"},
		{SenderPlayerID: 3, AcceptedMs: 300, Content: "平台没答"},
		{SenderPlayerID: 4, AcceptedMs: 400, Exhausted: true, Attempt: authority.PlatformRecheckMaxAttempts},
		{SenderPlayerID: 5, AcceptedMs: 500, Missing: true},
		{SenderPlayerID: 6, AcceptedMs: 600, AlreadyRetracted: true},
	}}
	pusher := &retractRecorder{}
	var checked []string
	sc := &ServiceContext{Authority: store, RootAdmission: drainproof.New(), WorldPush: pusher,
		PlatformCheck: func(_ context.Context, scene cgpb.TextScene, _ uint64, text string) contentguard.Result {
			require.Equal(t, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, scene)
			checked = append(checked, text)
			switch text {
			case "违规":
				return contentguard.Result{Verdict: contentguard.Reject}
			case "平台没答":
				return contentguard.Result{Verdict: contentguard.Unavailable}
			}
			return contentguard.Result{Verdict: contentguard.Pass}
		}}

	require.True(t, sc.runPlatformRecheckOnce(context.Background()))
	require.Equal(t, []string{"违规", "正常", "平台没答"}, checked, "只送真正要判的三条;放弃、原件不在、已撤回的不送平台")
	require.Equal(t, map[int64]authority.RecheckOutcome{
		100: authority.RecheckRetracted,
		200: authority.RecheckPassed,
		400: authority.RecheckGaveUp,
		500: authority.RecheckGaveUp,
		600: authority.RecheckRetracted,
	}, store.finished, "平台不可用的那条不记结局,留给下一次尝试")
	require.Equal(t, 1, store.retracts)
	require.Len(t, pusher.retracted, 1)
	require.EqualValues(t, 1, pusher.retracted[0].GetSenderPlayerId())
	require.EqualValues(t, 100, pusher.retracted[0].GetSendTimeMs())
	require.Equal(t, PlatformRejectReason, pusher.retracted[0].GetReason())
	require.Zero(t, sc.RootAdmission.Pending(), "每一次写做完都出了准入")
}

// 停机准入关门之后不再碰聊天库:不认领、不撤回,循环退出;待办留在库里给下一代副本。
func TestPlatformRecheckStopsWhenAdmissionCloses(t *testing.T) {
	store := &recheckStore{finished: map[int64]authority.RecheckOutcome{}, tasks: []authority.PlatformRecheckTask{{SenderPlayerID: 1, AcceptedMs: 100, Content: "违规"}}}
	sc := &ServiceContext{Authority: store, RootAdmission: drainproof.New(),
		PlatformCheck: func(context.Context, cgpb.TextScene, uint64, string) contentguard.Result {
			t.Fatal("the platform must not be called after admission closed")
			return contentguard.Result{}
		}}
	sc.RootAdmission.CloseAdmission()
	require.False(t, sc.runPlatformRecheckOnce(context.Background()))
	require.Len(t, store.tasks, 1, "关门之后不认领")
	require.Empty(t, store.finished)
}
