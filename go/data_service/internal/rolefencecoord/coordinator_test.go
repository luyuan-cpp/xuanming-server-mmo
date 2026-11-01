package rolefencecoord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/config"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var testSecret = strings.Repeat("c", 32)

var testState = &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: 2, HomeZone: 1}

type fakeParticipant struct {
	rfpb.RoleFenceParticipantClient
	mu         sync.Mutex
	response   *rfpb.PrepareRoleFreezeResponse
	applyRetry bool
	err        error
	hang       bool
	prepared   []*rfpb.PrepareRoleFreezeRequest
	applied    []*rfpb.ApplyRoleFenceRequest
}

func (f *fakeParticipant) PrepareRoleFreeze(ctx context.Context, in *rfpb.PrepareRoleFreezeRequest, _ ...grpc.CallOption) (*rfpb.PrepareRoleFreezeResponse, error) {
	f.mu.Lock()
	f.prepared = append(f.prepared, in)
	f.mu.Unlock()
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.response, f.err
}

func (f *fakeParticipant) ApplyRoleFence(_ context.Context, in *rfpb.ApplyRoleFenceRequest, _ ...grpc.CallOption) (*rfpb.ApplyRoleFenceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, in)
	return &rfpb.ApplyRoleFenceResponse{Retry: f.applyRetry}, f.err
}

func clean() *fakeParticipant {
	return &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Clean: true}}
}

func coordinatorFor(t *testing.T, clients ...*fakeParticipant) *Coordinator {
	t.Helper()
	c := newCoordinator(testSecret)
	for i, client := range clients {
		c.participants = append(c.participants, participant{name: rolefence.Participants[i], client: client})
	}
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

func TestPrepareAllCleanSignsEveryRequest(t *testing.T) {
	clients := []*fakeParticipant{clean(), clean(), clean()}
	c := coordinatorFor(t, clients...)
	require.Equal(t, rolefence.PrepareClean, c.Prepare(context.Background(), testState))
	auth, err := serviceauth.New(map[string]string{rolefence.CoordinatorCaller: testSecret})
	require.NoError(t, err)
	for _, client := range clients {
		require.Len(t, client.prepared, 1)
		require.True(t, proto.Equal(testState, client.prepared[0].State))
		caller, err := auth.Verify(context.Background(), rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, client.prepared[0])
		require.NoError(t, err)
		require.Equal(t, rolefence.CoordinatorCaller, caller)
	}
}

func TestPrepareMergesParticipantAnswers(t *testing.T) {
	blocked := func() *fakeParticipant {
		return &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Blocker: "guild_member"}}
	}
	stale := func() *fakeParticipant {
		return &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Stale: true}}
	}
	unreachable := func() *fakeParticipant { return &fakeParticipant{err: errors.New("down")} }
	retry := func() *fakeParticipant {
		return &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Retry: true}}
	}
	mute := func() *fakeParticipant { return &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{}} }
	for _, tc := range []struct {
		name    string
		clients []*fakeParticipant
		want    rolefence.PrepareOutcome
	}{
		{"有挂起", []*fakeParticipant{clean(), blocked()}, rolefence.PrepareBlocked},
		{"有挂起优先于不可达", []*fakeParticipant{unreachable(), blocked(), clean()}, rolefence.PrepareBlocked},
		{"不可达不算通过", []*fakeParticipant{clean(), unreachable()}, rolefence.PrepareRetry},
		{"参与者内部失败当不可达", []*fakeParticipant{clean(), retry()}, rolefence.PrepareRetry},
		{"过时不算通过", []*fakeParticipant{clean(), stale()}, rolefence.PrepareRetry},
		{"什么都没说不算通过,也不清令牌", []*fakeParticipant{clean(), mute()}, rolefence.PrepareRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, coordinatorFor(t, tc.clients...).Prepare(context.Background(), testState))
		})
	}
}

// 试探带 dry_run,也签名;真正的 Prepare 不带。结论的合并规则相同,问不齐不能算没有挂起。
func TestDryRunMarksEveryRequestAndMergesTheSameWay(t *testing.T) {
	clients := []*fakeParticipant{clean(), clean()}
	c := coordinatorFor(t, clients...)
	require.Equal(t, rolefence.PrepareClean, c.DryRun(context.Background(), testState))
	auth, err := serviceauth.New(map[string]string{rolefence.CoordinatorCaller: testSecret})
	require.NoError(t, err)
	for _, client := range clients {
		require.Len(t, client.prepared, 1)
		require.True(t, client.prepared[0].DryRun)
		_, err := auth.Verify(context.Background(), rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, client.prepared[0])
		require.NoError(t, err)
	}
	require.Equal(t, rolefence.PrepareClean, c.Prepare(context.Background(), testState))
	require.False(t, clients[0].prepared[1].DryRun)

	blocked := &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Blocker: "guild_member"}}
	require.Equal(t, rolefence.PrepareBlocked, coordinatorFor(t, clean(), blocked).DryRun(context.Background(), testState))
	require.Equal(t, rolefence.PrepareRetry, coordinatorFor(t, clean(), &fakeParticipant{err: errors.New("down")}).DryRun(context.Background(), testState))
	require.Equal(t, rolefence.PrepareRetry, coordinatorFor(t, clean(), &fakeParticipant{response: &rfpb.PrepareRoleFreezeResponse{Retry: true}}).DryRun(context.Background(), testState))
}

// 参与者挂住时要在 ctx 到期后返回,而且不能挡住别的参与者。
func TestPrepareIsParallelAndBounded(t *testing.T) {
	other := clean()
	c := coordinatorFor(t, &fakeParticipant{hang: true}, other)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.Equal(t, rolefence.PrepareRetry, c.Prepare(ctx, testState))
	require.Less(t, time.Since(start), prepareTimeout)
	require.Len(t, other.prepared, 1)
}

func TestPrepareOutcomeZeroValueIsNotClean(t *testing.T) {
	var zero rolefence.PrepareOutcome
	require.Equal(t, rolefence.PrepareRetry, zero, "goroutine 没跑完时结论是零值,不能算通过")
	require.Equal(t, rolefence.PrepareRetry, mergeOutcomes([]rolefence.PrepareOutcome{rolefence.PrepareClean, zero}))
}

// 推送不跟着请求的 ctx 走,丢了也不报错。Close 等在途推送结束。
func TestPushReachesAllAndIgnoresFailures(t *testing.T) {
	ok, broken, retrying := clean(), &fakeParticipant{err: errors.New("down")}, &fakeParticipant{applyRetry: true}
	c := newCoordinator(testSecret)
	c.participants = []participant{{name: "guild", client: ok}, {name: "trade", client: broken}, {name: "mail", client: retrying}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Push(ctx, testState)
	c.pushes.Wait()
	require.Len(t, ok.applied, 1)
	require.Len(t, broken.applied, 1)
	require.Len(t, retrying.applied, 1)
	auth, err := serviceauth.New(map[string]string{rolefence.CoordinatorCaller: testSecret})
	require.NoError(t, err)
	_, err = auth.Verify(context.Background(), rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName, ok.applied[0])
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestNewRequiresFullConfigAndSecret(t *testing.T) {
	_, err := New(config.RoleFenceConfig{}, testSecret)
	require.Error(t, err, "缺配置时不能有协调者,否则过户和删角会跳过参与者")
	var full config.RoleFenceConfig
	for _, name := range rolefence.Participants {
		full.Participants = append(full.Participants, config.RoleFenceParticipantConfig{Name: name, Endpoints: []string{"127.0.0.1:1"}})
	}
	_, err = New(full, "short")
	require.Error(t, err)
	c, err := New(full, testSecret)
	require.NoError(t, err, "连接是非阻塞的,参与者没起来不影响启动")
	require.Len(t, c.participants, len(rolefence.Participants))
	require.NoError(t, c.Close())
}

// fakeLocalServer 是同进程的参与者。
type fakeLocalServer struct {
	rfpb.UnimplementedRoleFenceParticipantServer
	response *rfpb.PrepareRoleFreezeResponse
	prepared []*rfpb.PrepareRoleFreezeRequest
	applied  []*rfpb.ApplyRoleFenceRequest
}

func (f *fakeLocalServer) PrepareRoleFreeze(_ context.Context, in *rfpb.PrepareRoleFreezeRequest) (*rfpb.PrepareRoleFreezeResponse, error) {
	f.prepared = append(f.prepared, in)
	return f.response, nil
}

func (f *fakeLocalServer) ApplyRoleFence(_ context.Context, in *rfpb.ApplyRoleFenceRequest) (*rfpb.ApplyRoleFenceResponse, error) {
	f.applied = append(f.applied, in)
	return &rfpb.ApplyRoleFenceResponse{}, nil
}

// 流水库的 GM 回收和别的参与者同一个判据:它有挂起,过户就不能过。
func TestLocalParticipantBlocksLikeARemoteOne(t *testing.T) {
	c := coordinatorFor(t, clean())
	local := &fakeLocalServer{response: &rfpb.PrepareRoleFreezeResponse{Blocker: "gm_recall_op"}}
	c.AddLocalParticipant(local)
	require.Equal(t, rolefence.PrepareBlocked, c.Prepare(context.Background(), testState))
	require.Equal(t, rolefence.PrepareBlocked, c.DryRun(context.Background(), testState))
	require.Len(t, local.prepared, 2)

	auth, err := serviceauth.New(map[string]string{rolefence.CoordinatorCaller: testSecret})
	require.NoError(t, err)
	caller, err := auth.Verify(context.Background(), rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, local.prepared[0])
	require.NoError(t, err, "进程内调用同样带签名,参与者的校验不开后门")
	require.Equal(t, rolefence.CoordinatorCaller, caller)

	local.response = &rfpb.PrepareRoleFreezeResponse{Clean: true}
	require.Equal(t, rolefence.PrepareClean, c.Prepare(context.Background(), testState))
}

func TestPushReachesLocalParticipant(t *testing.T) {
	c := coordinatorFor(t, clean())
	local := &fakeLocalServer{response: &rfpb.PrepareRoleFreezeResponse{Clean: true}}
	c.AddLocalParticipant(local)
	c.Push(context.Background(), testState)
	c.pushes.Wait()
	require.Len(t, local.applied, 1)
}
