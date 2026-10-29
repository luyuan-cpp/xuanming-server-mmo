package authority

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

func nativeDirectGroup(t *testing.T, f *nativeSpectatorFixture) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord) {
	t.Helper()
	actor12, _, _ := nativeRoleJoin(t, f.store, f.actor, 12)
	ticket12, err := f.store.Ticket(actor12, 12)
	require.NoError(t, err)
	require.NoError(t, f.store.CancelIntent(actor12, 12, ticket12.Ticket, ticket12.RequestId))
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	profile := &pb.MatchNetworkProfile{PlanId: "direct-observer-plan", SessionBinding: "original", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PhysicalRegion: 1, ExpiresAtMs: stamp + 120000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.store.SavePlan(f.actor, 11, profile, stamp+120000))
	formation := &pb.MatchFormationRecord{BattleId: 701, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: 5, ConfigId: 1}
	group := []*pb.MatchAuthorityTicketRecord{originalPlayer(f.actor, 11), originalPlayer(actor12, 12)}
	for _, ticket := range group {
		ticket.Ticket = uuid.NewString()
		ticket.RequestId = formation.OperationId
		ticket.Mode = formation.Mode
		ticket.ConfigId = 1
		ticket.TeamSize = 2
	}
	return formation, group
}
func TestDirectTiDBCapturesOriginalExitAndProfileThenRecoversWithoutTouchingNewWatch(t *testing.T) {
	f := nativeSpectator(t)
	watch := f.request()
	oldWatch := f.begin(t, watch)
	formation, group := nativeDirectGroup(t, f)
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: 101}
	body := []byte("original-guild-launch-and-roster")
	pending, err := f.store.PrepareDirectAdmission(f.actor, key, body, formation, group)
	require.NoError(t, err)
	require.Len(t, pending.SpectatorExits, 1)
	require.Equal(t, watch.RequestId, pending.SpectatorExits[0].OperationId)
	require.Zero(t, pending.BattleId)
	captured, err := DecodeDirectStart(pending)
	require.NoError(t, err)
	for _, ticket := range captured.Tickets {
		actor := f.actor
		if ticket.PlayerId == 12 {
			actor = nativeRoleActor(t, f.actor, 12, "original-12", 7)
		}
		actual, err := f.store.Ticket(actor, ticket.PlayerId)
		require.NoError(t, err)
		require.Equal(t, uint32(0), actual.State)
		require.True(t, TicketOccupied(actual))
	}
	_, err = f.store.CompletePreparedDirect(f.actor, pending)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	replay, err := f.store.PrepareDirectAdmission(f.actor, key, body, nil, nil)
	require.NoError(t, err)
	require.Equal(t, pending.StartOriginal, replay.StartOriginal)
	// 独立探测刷新不改变原pending测量；完成仍只能使用首次原profile。
	actor12 := nativeRoleActor(t, f.actor, 12, "original-12", 7)
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	replacement := &pb.MatchNetworkProfile{PlanId: "later-plan", SessionBinding: "later", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 12, PhysicalRegion: 2, ExpiresAtMs: stamp + 120000}, RttMs: 10, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.store.SavePlan(actor12, 12, replacement, stamp+120000))
	ok, err := f.store.AdvanceSpectator(context.Background(), oldWatch, 5)
	require.NoError(t, err)
	require.True(t, ok)
	later := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, later)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = f.store.BeginSpectator(f.actor, 11, later, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	result, err := f.store.CompletePreparedDirect(context.Background(), pending)
	require.NoError(t, err)
	require.Equal(t, formation.BattleId, result.BattleId)
	actual, err := f.store.Ticket(actor12, 12)
	require.NoError(t, err)
	require.Equal(t, captured.Tickets[1].NetworkProfile, actual.NetworkProfile)
	work, err := f.store.AcquireWork(f.actor, result.BattleId)
	require.NoError(t, err)
	require.NoError(t, f.store.Finish(f.actor, work, false))
	// pending期间的Intent已回滚；空闲后先沿正式入口建立later的head与原件。
	_, err = f.store.SpectatorIntent(f.actor, 11, later)
	require.NoError(t, err)
	current, err := f.store.BeginSpectator(f.actor, 11, later, f.formation, f.add)
	require.NoError(t, err)
	result, err = f.store.ReserveDirect(f.actor, key, body, nil, nil)
	require.NoError(t, err)
	require.Equal(t, uint32(5), result.State)
	actualWatch, err := f.store.SpectatorForActor(f.actor, 11, later.RequestId)
	require.NoError(t, err)
	require.True(t, proto.Equal(current, actualWatch))
}
func TestDirectTiDBPreparedOriginalRoleChangeRejectsBeforeFormation(t *testing.T) {
	f := nativeSpectator(t)
	formation, group := nativeDirectGroup(t, f)
	pending, err := f.store.PrepareDirectAdmission(f.actor, &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: 102}, []byte("original"), formation, group)
	require.NoError(t, err)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE account_role SET owner_revision=8 WHERE player_id=12")
	require.NoError(t, err)
	_, err = f.store.CompletePreparedDirect(context.Background(), pending)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	var count int
	require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT COUNT(*) FROM match_formation WHERE battle_id=701").Scan(&count))
	require.Zero(t, count)
}
func TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs(t *testing.T) {
	s := nativeRoleStore(t)
	var through DirectCursor
	for batch := uint64(1); batch <= 3; batch++ {
		actorA, a, _ := nativeRoleJoin(t, s, context.Background(), 30+batch*2)
		actorB, b, _ := nativeRoleJoin(t, s, context.Background(), 31+batch*2)
		require.NoError(t, s.CancelIntent(actorA, a.PlayerId, a.Ticket, a.RequestId))
		require.NoError(t, s.CancelIntent(actorB, b.PlayerId, b.Ticket, b.RequestId))
		formation := &pb.MatchFormationRecord{BattleId: 800 + batch, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
		_, err := s.PrepareDirectAdmission(context.Background(), &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: batch}, []byte("original"), formation, []*pb.MatchAuthorityTicketRecord{a, b})
		require.NoError(t, err)
		if batch == 2 {
			through, err = s.DirectAdmissionHighWater(context.Background())
			require.NoError(t, err)
		}
	}
	rows, err := s.DirectAdmissionsThrough(context.Background(), DirectCursor{}, through)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, uint64(1), rows[0].RequestLow)
	require.Equal(t, uint64(2), rows[1].RequestLow)
}
