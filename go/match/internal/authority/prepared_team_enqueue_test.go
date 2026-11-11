package authority

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

// 带观战退出原件的整队开战先停在 state 1,退出关闭后由 ReserveSQLTeamMatch 续推:补位单与真人 PVP 进队列
// (EnqueuePreparedTeamLaunch),满员 PVE 直接成局。进队列这一步原先只认补位(目标人数 > 成员数),
// 满员一侧的真人 PVP 被拒成 ErrDirectConflict,原件永远停在 state 1、队伍的开战锁一直占着,后台恢复每 500ms 重试一次。
//
// 不连库:沿用 client_write_deadline_test.go 的录制驱动,在准入之后的第一条写(清队伍开战锁)上取消请求 ctx,
// 让事务停在那里。走到这条写就说明过了准入;同时证明这一步跑过截止也不发 COMMIT。
// 进队列之后的票据与队列写由真库用例 TestTeamTiDBPreparedLaunchCapturesOnlyOriginalSpectatorAndResumesWholeQueue 覆盖。
func TestEnqueuePreparedTeamLaunchAdmitsFullPvpParty(t *testing.T) {
	cases := []struct {
		name     string
		mode     pb.MatchMode
		admitted bool
	}{
		{"full PVP side queues for opponents", pb.MatchMode_MATCH_MODE_PVP_TEAM, true},
		{"full PVE party never queues", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			ctx, cancel := context.WithCancel(sqlTraceActor(t, 11))
			defer cancel()

			launch, _, team := sqlTraceTeamLaunch(t, c.mode)
			teamPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(team)
			require.NoError(t, err)
			recorder.query = func(statement string) *sqlTraceRows {
				switch {
				case strings.HasPrefix(statement, "SELECT version,payload,state FROM team_authority WHERE team_id=? FOR UPDATE"):
					return sqlTraceRow(launch.TeamVersion+1, teamPayload, uint32(1))
				case strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=? FOR UPDATE"):
					return sqlTraceLaunchRow(launch)
				}
				return nil // 没有观战
			}
			recorder.onExec = func(statement string) {
				if strings.HasPrefix(statement, "UPDATE team_authority SET version=?") {
					cancel()
				}
			}

			err = s.EnqueuePreparedTeamLaunch(ctx, launch)
			require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
			statements := recorder.recorded()
			require.NotContains(t, statements, "COMMIT")
			if !c.admitted {
				require.ErrorIs(t, err, ErrDirectConflict)
				for _, statement := range statements {
					require.False(t, strings.HasPrefix(statement, "UPDATE team_authority"), "a full PVE party must not reach the queue writes: %v", statements)
				}
				return
			}
			requireDeadlineRefusal(t, err)
			requireReachedStatement(t, statements, "UPDATE team_authority SET version=?")
		})
	}
}
