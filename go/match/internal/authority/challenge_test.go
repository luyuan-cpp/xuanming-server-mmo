package authority

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	pb "proto/match"
	"shared/rolefence/fencetest"
	"shared/roleidentity"
)

func challengeActor(t *testing.T, id uint64, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: id}})
	require.NoError(t, err)
	return ctx
}

// 随机隔离TiDB内使用真实角色P/账号/修订核验，不复用独立匹配状态机的假guard。
func challengeStore(t *testing.T) (*Store, context.Context, context.Context) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	fencetest.Seed(t, ctx, s.DB, 1, "challenger", 3)
	fencetest.Seed(t, ctx, s.DB, 2, "target", 7)
	fencetest.Seed(t, ctx, s.DB, 3, "other", 1)
	require.NoError(t, s.ConfigureRoleFence(ctx, fencetest.ReplicaReader{DB: s.DB}, fencetest.Admissions{Valid: true}))
	return s, challengeActor(t, 1, "challenger", 3), challengeActor(t, 2, "target", 7)
}

func prepareChallengeGroup(t *testing.T, s *Store, challenger, target context.Context, config uint32) (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord) {
	t.Helper()
	n, err := s.Now(context.Background())
	require.NoError(t, err)
	for i, ctx := range []context.Context{challenger, target} {
		id := uint64(i + 1)
		profile := &pb.MatchNetworkProfile{PlanId: fmt.Sprintf("challenge-%d", id), SessionBinding: "original", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: id, PhysicalRegion: 1, ExpiresAtMs: n + 120000}, RttMs: 30, SampleCount: 3, MeasuredAtMs: n, ExpiresAtMs: n + 120000}}}
		require.NoError(t, s.SavePlan(ctx, id, profile, n+120000))
	}
	f := &pb.MatchFormationRecord{BattleId: 200, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 9, TargetInstanceId: "original-battle-instance", TargetEndpoint: "127.0.0.1:1", Mode: uint32(pb.MatchMode_MATCH_MODE_PVP_CHALLENGE), ConfigId: config}
	var group []*pb.MatchAuthorityTicketRecord
	for i, ctx := range []context.Context{challenger, target} {
		id := uint64(i + 1)
		original := originalPlayer(ctx, id)
		profile, _, err := s.Profile(ctx, id)
		require.NoError(t, err)
		payload, err := proto.Marshal(profile)
		require.NoError(t, err)
		original.Ticket, original.RequestId, original.Mode, original.ConfigId, original.TeamSize, original.NetworkProfile = uuid.NewString(), f.OperationId, f.Mode, config, 2, payload
		group = append(group, original)
	}
	return f, group
}

func TestChallengeTiDBNewInvitationFreezesBothOwnersAndReplaysOriginal(t *testing.T) {
	s, challenger, target := challengeStore(t)
	original, err := s.CreateChallenge(challenger, 100, 1, 2, 7, 60000)
	require.NoError(t, err)
	replayed, err := s.CreateChallenge(challenger, 101, 1, 2, 7, 60000)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, replayed), "重试不发新号、不刷新有效期")
	_, err = s.CreateChallenge(challenger, 102, 1, 2, 8, 60000)
	require.ErrorIs(t, err, ErrChallengePending)
	_, err = s.CreateChallenge(challengeActor(t, 3, "other", 1), 103, 3, 2, 7, 60000)
	require.ErrorIs(t, err, ErrChallengePending)
	stored, err := s.ChallengeForActor(target, 100, 2)
	require.NoError(t, err)
	require.Equal(t, "challenger", stored.ChallengerAccount)
	require.EqualValues(t, 7, stored.TargetRevision)
	fencetest.Update(t, context.Background(), s.DB, 2, "new-target", 8, 0)
	_, err = s.CreateChallenge(challenger, 104, 1, 2, 7, 60000)
	require.ErrorIs(t, err, ErrOriginalPlayer, "旧邀请不能随当前目录自动换target原件")
	_, err = s.ChallengeForActor(challengeActor(t, 2, "new-target", 8), 100, 2)
	require.ErrorIs(t, err, ErrOriginalPlayer)
}

func TestChallengeTiDBDeclineCannotBeReplayedAsAccept(t *testing.T) {
	s, challenger, target := challengeStore(t)
	invitation, err := s.CreateChallenge(challenger, 100, 1, 2, 7, 60000)
	require.NoError(t, err)
	formation, changed, err := s.DecideChallenge(target, invitation, false, nil, nil)
	require.NoError(t, err)
	require.Nil(t, formation)
	require.True(t, changed)
	_, changed, err = s.DecideChallenge(target, invitation, false, nil, nil)
	require.NoError(t, err)
	require.False(t, changed)
	_, _, err = s.DecideChallenge(target, invitation, true, nil, nil)
	require.ErrorIs(t, err, ErrDirectConflict)
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_formation").Scan(&count))
	require.Zero(t, count)
}

func TestChallengeTiDBAcceptanceRecoversSameFormationAndRejectsAlteredConfig(t *testing.T) {
	s, challenger, target := challengeStore(t)
	invitation, err := s.CreateChallenge(challenger, 100, 1, 2, 7, 60000)
	require.NoError(t, err)
	f, group := prepareChallengeGroup(t, s, challenger, target, 7)
	altered := proto.Clone(invitation).(*pb.MatchChallengeInvitationRecord)
	altered.ConfigId = 8
	_, _, err = s.DecideChallenge(target, altered, true, f, group)
	require.ErrorIs(t, err, ErrDirectConflict)
	found, changed, err := s.DecideChallenge(target, invitation, true, f, group)
	require.NoError(t, err)
	require.True(t, changed)
	require.Nil(t, found)
	prepared, err := s.DirectAdmission(target, &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: 2, RequestLow: 100})
	require.NoError(t, err)
	require.True(t, prepared.ExitPrepared)
	found, err = s.CompletePreparedDirect(target, prepared)
	require.NoError(t, err)
	require.Equal(t, f.BattleId, found.BattleId)
	// 新服务对象重读SQL，不持有旧实例内存；重复接受只回原formation。
	restarted := &Store{DB: s.DB, Config: s.Config}
	require.NoError(t, restarted.ConfigureRoleFence(context.Background(), fencetest.ReplicaReader{DB: s.DB}, fencetest.Admissions{Valid: true}))
	stored, err := restarted.ChallengeForActor(target, 100, 2)
	require.NoError(t, err)
	replayed, changed, err := restarted.DecideChallenge(target, stored, true, nil, nil)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, f.BattleId, replayed.BattleId)
	var formations, directs int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_formation").Scan(&formations))
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_direct_intent WHERE kind=3").Scan(&directs))
	require.Equal(t, 1, formations)
	require.Equal(t, 1, directs)
	fencetest.Update(t, context.Background(), s.DB, 2, "other-owner", 8, 0)
	_, _, err = restarted.DecideChallenge(target, stored, true, nil, nil)
	require.ErrorIs(t, err, ErrOriginalPlayer, "终态幂等也不能绕原owner围栏")
}

func TestChallengeTiDBConcurrentAcceptAndDeclineCommitOneDecision(t *testing.T) {
	s, challenger, target := challengeStore(t)
	invitation, err := s.CreateChallenge(challenger, 100, 1, 2, 7, 60000)
	require.NoError(t, err)
	f, group := prepareChallengeGroup(t, s, challenger, target, 7)
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, accept := range []bool{false, true} {
		workers.Add(1)
		go func(accept bool) {
			defer workers.Done()
			<-start
			_, _, err := s.DecideChallenge(target, invitation, accept, f, group)
			errors <- err
		}(accept)
	}
	close(start)
	workers.Wait()
	close(errors)
	successes := 0
	for err := range errors {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, ErrDirectConflict)
		}
	}
	require.Equal(t, 1, successes)
	var state uint32
	var battle uint64
	require.NoError(t, s.DB.QueryRow("SELECT state,battle_id FROM match_challenge_invitation WHERE challenge_id=100").Scan(&state, &battle))
	var formations int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_formation").Scan(&formations))
	if state == challengeAcceptedPending {
		require.Zero(t, battle)
		require.Zero(t, formations)
	} else {
		require.Equal(t, challengeDeclined, state)
		require.Zero(t, battle)
		require.Zero(t, formations)
	}
}

func TestChallengeTiDBExpiryIsCheckedBeforeDecisionSideEffects(t *testing.T) {
	s, challenger, target := challengeStore(t)
	invitation, err := s.CreateChallenge(challenger, 100, 1, 2, 7, 60000)
	require.NoError(t, err)
	_, err = s.DB.Exec("UPDATE match_challenge_invitation SET expires_ms=1 WHERE challenge_id=100")
	require.NoError(t, err)
	invitation.ExpiresMs = 1
	_, _, err = s.DecideChallenge(target, invitation, false, nil, nil)
	require.ErrorIs(t, err, ErrChallengeExpired)
	var directs int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_direct_intent WHERE kind=3").Scan(&directs))
	require.Zero(t, directs)
}
