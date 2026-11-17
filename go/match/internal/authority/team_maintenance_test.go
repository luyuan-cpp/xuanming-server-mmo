package authority

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

// GetQueueStatus 的票据读取可以先把某个成员的过期票结为 state4。
// 后台维护必须把同一原件的整队收尾，不能永久保留阻止下次匹配的 state2 launch。
func TestMaintainQueuedTeamClosesAfterMemberTicketReadExpires(t *testing.T) {
	for _, actorRead := range []bool{false, true} {
		name := "background ticket read"
		if actorRead {
			name = "authenticated ticket read"
		}
		t.Run(name, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			launch, group, team := sqlTraceTeamLaunch(t, pb.MatchMode_MATCH_MODE_PVP_TEAM)
			launch.State = 2
			for _, ticket := range group {
				ticket.Mode, ticket.ConfigId, ticket.TeamSize = launch.Mode, launch.ConfigId, launch.TargetSize
				ticket.State, ticket.ExpiresMs = 1, 2000
			}
			group[0].ExpiresMs = 500
			teamBytes, err := proto.Marshal(team)
			require.NoError(t, err)
			readingTicket, nextTicket := true, 0
			recorder.query = func(statement string) *sqlTraceRows {
				switch {
				case strings.HasPrefix(statement, "SELECT version,payload,state FROM team_authority"):
					return sqlTraceRow(launch.TeamVersion, teamBytes, uint32(1))
				case strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch"):
					return sqlTraceLaunchRow(launch)
				case strings.HasPrefix(statement, "SELECT CAST(UNIX_TIMESTAMP"):
					return sqlTraceRow(uint64(1000))
				case strings.HasPrefix(statement, "SELECT "+ticketColumns+" FROM match_authority_ticket"):
					if readingTicket {
						return sqlTraceTicketRow(group[0])
					}
					row := sqlTraceTicketRow(group[nextTicket])
					nextTicket++
					return row
				}
				return nil
			}
			recorder.onExec = func(statement string) {
				if readingTicket && strings.HasPrefix(statement, "UPDATE match_authority_ticket SET state=4") {
					group[0].State = 4
				}
				if strings.HasPrefix(statement, "UPDATE team_launch SET state=5") {
					launch.State = 5
				}
			}
			ctx := context.Background()
			if actorRead {
				ctx = sqlTraceActor(t, group[0].PlayerId)
			}
			ticket, err := s.Ticket(ctx, group[0].PlayerId)
			require.NoError(t, err)
			require.Equal(t, uint32(4), ticket.State)
			if actorRead {
				require.Equal(t, "COMMIT", recorder.awaitTxEnd(t))
			}
			readingTicket = false
			closed, err := s.MaintainQueuedTeam(context.Background(), launch)
			require.NoError(t, err)
			require.True(t, closed)
			require.Equal(t, "COMMIT", recorder.awaitTxEnd(t))
			require.Equal(t, uint32(5), launch.State)
			requireReachedStatement(t, recorder.recorded(), "UPDATE team_launch_role SET state=5")

			// 另一个副本或下一轮重复维护同一原件，只查询终态，不重新关票。
			closed, err = s.MaintainQueuedTeam(context.Background(), launch)
			require.NoError(t, err)
			require.False(t, closed)
			require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
		})
	}
}

func TestMaintainQueuedTeamRefusesDifferentOrFormedTicket(t *testing.T) {
	cases := []struct {
		name   string
		change func(*pb.MatchAuthorityTicketRecord)
	}{
		{"replacement ticket", func(v *pb.MatchAuthorityTicketRecord) { v.Ticket = "replacement" }},
		{"different owner", func(v *pb.MatchAuthorityTicketRecord) { v.OwnerRevision++ }},
		{"different team", func(v *pb.MatchAuthorityTicketRecord) { v.TeamId++ }},
		{"different mode", func(v *pb.MatchAuthorityTicketRecord) { v.Mode++ }},
		{"different config", func(v *pb.MatchAuthorityTicketRecord) { v.ConfigId++ }},
		{"different size", func(v *pb.MatchAuthorityTicketRecord) { v.TeamSize++ }},
		{"formation pending", func(v *pb.MatchAuthorityTicketRecord) { v.State, v.FormationId = 2, 9001 }},
		{"room created", func(v *pb.MatchAuthorityTicketRecord) { v.State, v.FormationId = 3, 9001 }},
		{"terminal formed ticket", func(v *pb.MatchAuthorityTicketRecord) { v.FormationId = 9001 }},
		{"unknown state", func(v *pb.MatchAuthorityTicketRecord) { v.State = 99 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			launch, group, team := sqlTraceTeamLaunch(t, pb.MatchMode_MATCH_MODE_PVP_TEAM)
			launch.State = 2
			ticket := group[0]
			ticket.Mode, ticket.ConfigId, ticket.TeamSize = launch.Mode, launch.ConfigId, launch.TargetSize
			ticket.State, ticket.ExpiresMs = 4, 500
			tc.change(ticket)
			teamBytes, err := proto.Marshal(team)
			require.NoError(t, err)
			recorder.query = func(statement string) *sqlTraceRows {
				switch {
				case strings.HasPrefix(statement, "SELECT version,payload,state FROM team_authority"):
					return sqlTraceRow(launch.TeamVersion, teamBytes, uint32(1))
				case strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch"):
					return sqlTraceLaunchRow(launch)
				case strings.HasPrefix(statement, "SELECT CAST(UNIX_TIMESTAMP"):
					return sqlTraceRow(uint64(1000))
				case strings.HasPrefix(statement, "SELECT "+ticketColumns+" FROM match_authority_ticket"):
					return sqlTraceTicketRow(ticket)
				}
				return nil
			}
			closed, err := s.MaintainQueuedTeam(context.Background(), launch)
			require.ErrorIs(t, err, ErrDirectConflict)
			require.False(t, closed)
			require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
			for _, statement := range recorder.recorded() {
				require.False(t, strings.HasPrefix(statement, "UPDATE "), "must not change a different operation: %s", statement)
			}
		})
	}
}
