package data

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "proto/guild"
	matchpb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeTrialOccupancy 是 match 上占用的替身。判据本身在 go/match/internal/authority/occupancy_native_test.go。
type fakeTrialOccupancy struct {
	mu         sync.Mutex
	acquireErr error
	acquires   []fakeAcquire
	releases   []uint64
	// onAcquire 在下一次 Acquire 之前跑一次,摆「续租排在别人结束大厅之后」这种交错。
	onAcquire func()
}

func (f *fakeTrialOccupancy) beforeAcquire(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onAcquire = hook
}

type fakeAcquire struct {
	owner  uint64
	roster []*pb.GuildTrialRoleIdentity
	ttl    time.Duration
}

func (f *fakeTrialOccupancy) Acquire(_ context.Context, owner uint64, roster []*pb.GuildTrialRoleIdentity, ttl time.Duration) error {
	f.mu.Lock()
	hook := f.onAcquire
	f.onAcquire = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires = append(f.acquires, fakeAcquire{owner: owner, roster: roster, ttl: ttl})
	return f.acquireErr
}

func (f *fakeTrialOccupancy) Release(_ context.Context, owner uint64, _ []uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, owner)
	return nil
}

func (f *fakeTrialOccupancy) setAcquireErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireErr = err
}

func (f *fakeTrialOccupancy) acquireCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.acquires)
}

func (f *fakeTrialOccupancy) lastAcquire() fakeAcquire {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquires[len(f.acquires)-1]
}

func (f *fakeTrialOccupancy) released() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.releases...)
}

func occupancyOf(repo *GuildRepo) *fakeTrialOccupancy {
	return repo.occupancy.(*fakeTrialOccupancy)
}

// requireSameRoster 逐个 proto.Equal:消息内部有反射缓存,不能整体比结构体。
func requireSameRoster(t *testing.T, want, got []*pb.GuildTrialRoleIdentity, message string) {
	t.Helper()
	require.Len(t, got, len(want), message)
	for i := range want {
		require.True(t, proto.Equal(want[i], got[i]), "%s: 第 %d 个身份 %v != %v", message, i, want[i], got[i])
	}
}

var testMatchSecret = strings.Repeat("m", 32)

// fakeMatchInternal 核 guild 的签名,回用例给的结论。
type fakeMatchInternal struct {
	matchpb.MatchInternalClient
	t           *testing.T
	acquireOut  *matchpb.AcquireTrialOccupancyResponse
	releaseOut  *matchpb.ReleaseTrialOccupancyResponse
	readOut     *matchpb.GetBattleCompletionResponse
	err         error
	lastAcquire *matchpb.AcquireTrialOccupancyRequest
	lastRelease *matchpb.ReleaseTrialOccupancyRequest
}

func (f *fakeMatchInternal) verify(ctx context.Context, method string, in proto.Message) {
	f.t.Helper()
	verifier, err := serviceauth.New(map[string]string{"guild": testMatchSecret})
	require.NoError(f.t, err)
	caller, err := verifier.Verify(ctx, method, in)
	require.NoError(f.t, err)
	require.Equal(f.t, "guild", caller)
}

func (f *fakeMatchInternal) AcquireTrialOccupancy(ctx context.Context, in *matchpb.AcquireTrialOccupancyRequest, _ ...grpc.CallOption) (*matchpb.AcquireTrialOccupancyResponse, error) {
	f.verify(ctx, matchpb.MatchInternal_AcquireTrialOccupancy_FullMethodName, in)
	f.lastAcquire = in
	return f.acquireOut, f.err
}

func (f *fakeMatchInternal) ReleaseTrialOccupancy(ctx context.Context, in *matchpb.ReleaseTrialOccupancyRequest, _ ...grpc.CallOption) (*matchpb.ReleaseTrialOccupancyResponse, error) {
	f.verify(ctx, matchpb.MatchInternal_ReleaseTrialOccupancy_FullMethodName, in)
	f.lastRelease = in
	return f.releaseOut, f.err
}

func (f *fakeMatchInternal) GetBattleCompletion(ctx context.Context, in *matchpb.GetBattleCompletionRequest, _ ...grpc.CallOption) (*matchpb.GetBattleCompletionResponse, error) {
	f.verify(ctx, matchpb.MatchInternal_GetBattleCompletion_FullMethodName, in)
	return f.readOut, f.err
}

func TestMatchOccupancyClientMapsConclusions(t *testing.T) {
	roster := []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22}}
	for _, tc := range []struct {
		name string
		out  *matchpb.AcquireTrialOccupancyResponse
		err  error
		want error
	}{
		{name: "占到了", out: &matchpb.AcquireTrialOccupancyResponse{}},
		{name: "票或别的大厅占着", out: &matchpb.AcquireTrialOccupancyResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_OCCUPIED, OffenderPlayerId: 22}, want: ErrWriteConflict},
		{name: "角色冻结或换主", out: &matchpb.AcquireTrialOccupancyResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_ROLE}, want: ErrWriteConflict},
		{name: "这次没结论", out: &matchpb.AcquireTrialOccupancyResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}, want: ErrTrialMatchUnavailable},
		{name: "验签失败", out: &matchpb.AcquireTrialOccupancyResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED}, want: ErrTrialMatchUnavailable},
		{name: "调不通", err: status.Error(codes.Unavailable, "down"), want: ErrTrialMatchUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &fakeMatchInternal{t: t, acquireOut: tc.out, err: tc.err}
			client, err := NewMatchOccupancyClient(rpc, testMatchSecret)
			require.NoError(t, err)
			err = client.Acquire(context.Background(), 71, roster, 90*time.Second)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.EqualValues(t, 71, rpc.lastAcquire.OwnerId)
			require.EqualValues(t, 90000, rpc.lastAcquire.TtlMs)
			require.Len(t, rpc.lastAcquire.Roster, 2)
			require.Equal(t, "seller", rpc.lastAcquire.Roster[0].Account)
			require.Zero(t, rpc.lastAcquire.Roster[1].OwnerRevision, "对端身份留空,由 match 捕获")
		})
	}
	_, err := NewMatchOccupancyClient(nil, testMatchSecret)
	require.Error(t, err)
	_, err = NewMatchOccupancyClient(&fakeMatchInternal{t: t}, "short")
	require.Error(t, err)
}

func TestMatchOccupancyClientReleaseReportsFailures(t *testing.T) {
	rpc := &fakeMatchInternal{t: t, releaseOut: &matchpb.ReleaseTrialOccupancyResponse{}}
	client, err := NewMatchOccupancyClient(rpc, testMatchSecret)
	require.NoError(t, err)
	require.NoError(t, client.Release(context.Background(), 71, []uint64{11, 22}))
	require.Equal(t, []uint64{11, 22}, rpc.lastRelease.PlayerIds)
	rpc.releaseOut = &matchpb.ReleaseTrialOccupancyResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}
	require.Error(t, client.Release(context.Background(), 71, []uint64{11}))
	rpc.err = errors.New("down")
	require.Error(t, client.Release(context.Background(), 71, []uint64{11}))
}

// 没绑占用(没配 MatchRpc):拿不到就是问不到结论,放开是空操作。
func TestTrialOccupancyUnboundIsUnavailable(t *testing.T) {
	repo := &GuildRepo{}
	require.ErrorIs(t, repo.acquireTrialOccupancy(context.Background(), 71, nil, time.Second), ErrTrialMatchUnavailable)
	repo.releaseTrialOccupancy(context.Background(), 71, nil)
}

// 放开不继承请求的取消:这一步常在请求预算见底之后才走到。失败只记日志。
func TestReleaseTrialOccupancyIgnoresRequestCancellation(t *testing.T) {
	occupancy := &fakeCancelCheckingOccupancy{}
	repo := &GuildRepo{occupancy: occupancy}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	repo.releaseTrialOccupancy(cancelled, 71, []uint64{11})
	require.True(t, occupancy.called)
	require.NoError(t, occupancy.callErr)
}

type fakeCancelCheckingOccupancy struct {
	called  bool
	callErr error
}

func (f *fakeCancelCheckingOccupancy) Acquire(context.Context, uint64, []*pb.GuildTrialRoleIdentity, time.Duration) error {
	return nil
}

func (f *fakeCancelCheckingOccupancy) Release(ctx context.Context, _ uint64, _ []uint64) error {
	f.called, f.callErr = true, ctx.Err()
	return errors.New("release failed")
}

// 巡检器读 match 的 SQL 终局:COMPLETE 原样读回原字节(protobuf 字节,不是文本);ABORT 没有 battle 原字节,等于没有结果,
// 走超时语义;没有终局同样是没有结果;问不到必须报错,不能伪装成没有结果。
func TestTrialResultRecordsReadMatchTerminal(t *testing.T) {
	_, err := NewTrialResultRecords(nil, testMatchSecret)
	require.Error(t, err)
	_, err = NewTrialResultRecords(&fakeMatchInternal{t: t}, "short")
	require.Error(t, err, "没配密钥不能装配")

	rpc := &fakeMatchInternal{t: t}
	records, err := NewTrialResultRecords(rpc, testMatchSecret)
	require.NoError(t, err)
	ctx := context.Background()

	payload := []byte{0x08, 0x00, 0xff, 0x10, 0x01}
	rpc.readOut = &matchpb.GetBattleCompletionResponse{Decision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, RawPayload: payload}
	got, found, err := records.Load(ctx, 9_007_199_254_741_001)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, payload, got)

	for _, decision := range []matchpb.BattleOutcomeDecision{matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN} {
		rpc.readOut = &matchpb.GetBattleCompletionResponse{Decision: decision}
		_, found, err = records.Load(ctx, 9_007_199_254_741_002)
		require.NoError(t, err)
		require.False(t, found, "decision=%v", decision)
	}
	_, _, err = records.Load(ctx, 0)
	require.Error(t, err)

	rpc.readOut = &matchpb.GetBattleCompletionResponse{Reject: matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}
	_, found, err = records.Load(ctx, 9_007_199_254_741_001)
	require.Error(t, err, "读不到不是没有结果")
	require.False(t, found)
	rpc.readOut, rpc.err = nil, status.Error(codes.Unavailable, "down")
	_, found, err = records.Load(ctx, 9_007_199_254_741_001)
	require.Error(t, err, "读不到不是没有结果")
	require.False(t, found)
}
