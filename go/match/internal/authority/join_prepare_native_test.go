package authority

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

func spectatorJoinRequest(t *testing.T, f *nativeSpectatorFixture) (*pb.JoinQueueRequest, *pb.MatchAuthorityTicketRecord) {
	t.Helper()
	n, err := f.store.Now(f.actor)
	require.NoError(t, err)
	plan := "spectator-join-plan"
	profile := &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: "spectator-bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PlanId: plan, SessionBinding: "spectator-bound", PhysicalRegion: 1, ExpiresAtMs: n + 120000}, RttMs: 50, SampleCount: 3, MeasuredAtMs: n, ExpiresAtMs: n + 120000}}}
	require.NoError(t, f.store.SavePlan(f.actor, 11, profile, n+120000))
	request := &pb.JoinQueueRequest{RequestId: uuid.NewString(), NetworkPlanId: plan, Mode: pb.MatchMode(3), BattleConfigId: 1}
	return request, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: request.RequestId, Mode: 3, ConfigId: 1, TeamSize: 2, ZoneId: 1}
}

func TestJoinTiDBPreparedExitNeedsPositiveProofAndReplayNeverClosesNewWatch(t *testing.T) {
	f := nativeSpectator(t)
	watch := f.begin(t, f.request())
	_, err := f.store.AdvanceSpectator(f.actor, watch, 2)
	require.NoError(t, err)
	request, offered := spectatorJoinRequest(t, f)
	original, exit, pending, err := f.store.PrepareJoin(f.actor, offered, request, "spectator-bound")
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, watch.OperationId, exit.OperationId)
	_, _, err = f.store.Join(f.actor, original, request.NetworkPlanId, "spectator-bound")
	require.ErrorIs(t, err, ErrSpectatorBusy)
	offered.Ticket = uuid.NewString()
	replayed, sameExit, pending, err := f.store.PrepareJoin(f.actor, offered, request, "spectator-bound")
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, original.Ticket, replayed.Ticket)
	require.True(t, proto.Equal(exit, sameExit))
	_, err = f.store.AdvanceSpectator(f.actor, exit, 5)
	require.NoError(t, err)
	newWatchRequest := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, newWatchRequest)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = f.store.BeginSpectator(f.actor, 11, newWatchRequest, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	queued, created, err := f.store.Join(f.actor, original, request.NetworkPlanId, "spectator-bound")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, original.Ticket, queued.Ticket)
	require.NoError(t, f.store.CancelIntent(f.actor, 11, queued.Ticket, queued.RequestId))
	// 用另一已完成的Join检查旧成功原件回放；取消原UUID本身应始终拒绝。
	request2, offered2 := spectatorJoinRequest(t, f)
	prepared2, _, _, err := f.store.PrepareJoin(f.actor, offered2, request2, "spectator-bound")
	require.NoError(t, err)
	queued2, _, err := f.store.Join(f.actor, prepared2, request2.NetworkPlanId, "spectator-bound")
	require.NoError(t, err)
	_, peer, _ := nativeRoleJoin(t, f.store, f.actor, 22)
	failed := &pb.MatchFormationRecord{BattleId: 701, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
	err = f.store.Claim(f.actor, failed, []*pb.MatchAuthorityTicketRecord{queued2, peer})
	require.NoError(t, err)
	work, err := f.store.AcquireWork(f.actor, failed.BattleId)
	require.NoError(t, err)
	require.NoError(t, f.store.Finish(f.actor, work, false))
	newWatch := f.begin(t, f.request())
	_, err = f.store.AdvanceSpectator(f.actor, newWatch, 2)
	require.NoError(t, err)
	_, oldExit, pending, err := f.store.PrepareJoin(f.actor, offered2, request2, "spectator-bound")
	require.NoError(t, err)
	require.False(t, pending)
	require.Nil(t, oldExit)
	_, _, err = f.store.Join(f.actor, queued2, request2.NetworkPlanId, "spectator-bound")
	require.NoError(t, err)
	stillWatching, err := f.store.SpectatorForActor(f.actor, 11, newWatch.OperationId)
	require.NoError(t, err)
	require.Equal(t, uint32(2), stillWatching.State)
}

func TestJoinTiDBPreparedCancellationAndOwnerABARejectLateCompletion(t *testing.T) {
	f := nativeSpectator(t)
	request, offered := spectatorJoinRequest(t, f)
	prepared, _, _, err := f.store.PrepareJoin(f.actor, offered, request, "spectator-bound")
	require.NoError(t, err)
	require.NoError(t, f.store.CancelIntent(f.actor, 11, "", request.RequestId))
	_, _, err = f.store.Join(f.actor, prepared, request.NetworkPlanId, "spectator-bound")
	require.ErrorIs(t, err, ErrJoinCancelled)
	current, err := f.store.Ticket(f.actor, 11)
	require.NoError(t, err)
	require.Equal(t, uint32(4), current.State)
	request, offered = spectatorJoinRequest(t, f)
	prepared, _, _, err = f.store.PrepareJoin(f.actor, offered, request, "spectator-bound")
	require.NoError(t, err)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE role_fence SET owner_revision=9,version=version+1 WHERE player_id=11")
	require.NoError(t, err)
	_, _, err = f.store.Join(f.actor, prepared, request.NetworkPlanId, "spectator-bound")
	require.ErrorIs(t, err, ErrOriginalPlayer)
}

func TestJoinTiDBPreparedProfileAndRequestAreImmutable(t *testing.T) {
	f := nativeSpectator(t)
	request, offered := spectatorJoinRequest(t, f)
	prepared, _, _, err := f.store.PrepareJoin(f.actor, offered, request, "spectator-bound")
	require.NoError(t, err)
	changed := proto.Clone(request).(*pb.JoinQueueRequest)
	changed.NetworkPlanId = "replaced-plan"
	_, _, _, err = f.store.PrepareJoin(f.actor, offered, changed, "spectator-bound")
	require.Error(t, err)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_network_profile SET payload=?,expires_ms=0 WHERE player_id=11", []byte("replaced"))
	require.NoError(t, err)
	queued, created, err := f.store.Join(f.actor, prepared, request.NetworkPlanId, "spectator-bound")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, prepared.NetworkProfile, queued.NetworkProfile)
}
