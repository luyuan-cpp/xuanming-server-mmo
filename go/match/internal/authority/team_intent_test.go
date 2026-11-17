package authority

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
	team "proto/team"
)

func TestTeamMatchIntentRecoversQueuedAndClosedOriginal(t *testing.T) {
	for _, state := range []uint32{1, 2, 5} {
		t.Run(map[uint32]string{1: "prepared", 2: "queued", 5: "closed without battle"}[state], func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			launch, _, _ := sqlTraceTeamLaunch(t, pb.MatchMode_MATCH_MODE_PVP_TEAM)
			launch.State = state
			data, err := DecodeTeamLaunch(launch)
			require.NoError(t, err)
			recorder.query = func(statement string) *sqlTraceRows {
				if strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch") {
					return sqlTraceLaunchRow(launch)
				}
				return nil
			}
			intent, err := s.TeamMatchIntent(sqlTraceActor(t, launch.ActorId), launch.ActorId, data.Original.Request.RequestId)
			require.NoError(t, err)
			require.True(t, proto.Equal(&team.TeamMatchIntent{RequestId: data.Original.Request.RequestId, State: state}, intent))
			for _, statement := range recorder.recorded() {
				require.False(t, strings.HasPrefix(statement, "UPDATE "), "recovery must not create or change an operation")
			}
		})
	}
}

func TestTeamMatchIntentPreservesOriginalFormationOutcome(t *testing.T) {
	for _, formationState := range []uint32{4, 5} {
		t.Run(map[uint32]string{4: "completed battle", 5: "aborted battle"}[formationState], func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			launch, _, _ := sqlTraceTeamLaunch(t, pb.MatchMode_MATCH_MODE_PVP_TEAM)
			launch.State, launch.BattleId = 5, 9001
			data, err := DecodeTeamLaunch(launch)
			require.NoError(t, err)
			recorder.query = func(statement string) *sqlTraceRows {
				switch {
				case strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch"):
					return sqlTraceLaunchRow(launch)
				case strings.HasPrefix(statement, "SELECT "+formationColumns+" FROM match_formation"):
					return sqlTraceRow(uint64(9001), "operation", formationState, uint32(1), uint32(1), "instance", "endpoint", uint32(7), uint32(1),
						[]byte{}, []byte{}, uint64(1), uint64(2), uint64(0), []byte{}, false, []byte{}, uint32(1), uint32(1), "instance", uint32(1), true, "stable")
				}
				return nil
			}
			intent, err := s.TeamMatchIntent(sqlTraceActor(t, launch.ActorId), launch.ActorId, data.Original.Request.RequestId)
			require.NoError(t, err)
			require.Equal(t, uint64(9001), intent.BattleId)
			require.Equal(t, formationState, intent.State, "closed launch must not turn a successful battle into a cancellation")
		})
	}
}

func TestTeamMatchIntentMissingNeedsConfirmedCancellation(t *testing.T) {
	for _, source := range []string{"missing", "cancel before launch", "legacy direct cancelled", "different owner"} {
		t.Run(source, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			launch, _, _ := sqlTraceTeamLaunch(t, pb.MatchMode_MATCH_MODE_PVP_TEAM)
			data, err := DecodeTeamLaunch(launch)
			require.NoError(t, err)
			recorder.query = func(statement string) *sqlTraceRows {
				if source == "different owner" && strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch") {
					launch.OwnerRevision++
					return sqlTraceLaunchRow(launch)
				}
				if source == "legacy direct cancelled" && strings.HasPrefix(statement, "SELECT battle_id,payload,cancelled,owner_account,owner_revision FROM match_direct_intent") {
					return sqlTraceRow(uint64(0), []byte{}, true, launch.OwnerAccount, launch.OwnerRevision)
				}
				if source == "cancel before launch" && strings.HasPrefix(statement, "SELECT cancelled,owner_account,owner_revision FROM match_join_intent") {
					return sqlTraceRow(true, launch.OwnerAccount, launch.OwnerRevision)
				}
				return nil
			}
			intent, err := s.TeamMatchIntent(sqlTraceActor(t, launch.ActorId), launch.ActorId, data.Original.Request.RequestId)
			switch source {
			case "different owner":
				require.ErrorIs(t, err, ErrOriginalPlayer)
				require.Nil(t, intent)
			case "missing":
				require.NoError(t, err)
				require.Nil(t, intent, "no committed original is not proof that a late request cannot still commit")
			default:
				require.NoError(t, err)
				require.Equal(t, uint32(5), intent.State)
				require.Zero(t, intent.BattleId)
				require.Equal(t, data.Original.Request.RequestId, intent.RequestId)
			}
		})
	}
}
