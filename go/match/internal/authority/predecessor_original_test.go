package authority

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "proto/match"
)

// 单人排队的原件(票、Join 意图)是角色过户前的主人留下的时候,现主人的查询与撤销要有确定结论,不能当故障回:
// 客户端的排队恢复只认「不会再入队」这一个出口,回故障它就永远停在排队中。不连 TiDB,用录制驱动。

const predecessorAccount = "previous-owner"

func rejectingRoleGuard(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord) error {
	return ErrOriginalPlayer
}

// GetQueueStatus 的 request_cancelled 口径:撤销墓碑,或请求号的原件属于前主人,都是「不会再入队」;
// 自己未撤销的原件、查不到原件都不是。会话本身已不归角色现主人时照旧报 ErrOriginalPlayer。
func TestQueueRequestClosedCoversTombstoneAndPredecessorOriginal(t *testing.T) {
	const player = uint64(11)
	cases := []struct {
		name   string
		intent func() *sqlTraceRows // match_join_intent 行:cancelled, owner_account, owner_revision;nil = 没有这一行
		closed bool
	}{
		{"own tombstone", func() *sqlTraceRows { return sqlTraceRow(true, sqlTraceAccount(player), uint64(7)) }, true},
		{"own live original", func() *sqlTraceRows { return sqlTraceRow(false, sqlTraceAccount(player), uint64(7)) }, false},
		{"predecessor original", func() *sqlTraceRows { return sqlTraceRow(false, predecessorAccount, uint64(6)) }, true},
		{"missing", func() *sqlTraceRows { return nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			recorder.query = func(statement string) *sqlTraceRows {
				if strings.HasPrefix(statement, "SELECT cancelled,owner_account,owner_revision FROM match_join_intent") {
					return tc.intent()
				}
				return nil
			}
			closed, err := s.QueueRequestClosed(sqlTraceActor(t, player), player, uuid.NewString())
			require.NoError(t, err)
			require.Equal(t, tc.closed, closed)
		})
	}
	t.Run("stale session", func(t *testing.T) {
		s := sqlTraceStore(t, newSQLTraceRecorder())
		s.roleGuard = rejectingRoleGuard
		closed, err := s.QueueRequestClosed(sqlTraceActor(t, player), player, uuid.NewString())
		require.ErrorIs(t, err, ErrOriginalPlayer)
		require.NotErrorIs(t, err, ErrForeignOriginal)
		require.False(t, closed)
	})
}

// CancelQueue 点名的票或请求号属于前主人:回成功,不写撤销墓碑、不动原票,事务回滚。
func TestCancelIntentLeavesPredecessorOriginalUntouched(t *testing.T) {
	const player = uint64(11)
	request := uuid.NewString()
	for _, source := range []string{"ticket", "join intent"} {
		t.Run(source, func(t *testing.T) {
			recorder := newSQLTraceRecorder()
			s := sqlTraceStore(t, recorder)
			recorder.query = func(statement string) *sqlTraceRows {
				switch {
				case strings.HasPrefix(statement, "SELECT "+ticketColumns+" FROM match_authority_ticket"):
					if source == "ticket" {
						return sqlTraceTicketRow(&pb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: "previous-ticket", RequestId: request,
							State: 4, OwnerAccount: predecessorAccount, OwnerRevision: 6})
					}
					return sqlTraceTicketRow(&pb.MatchAuthorityTicketRecord{PlayerId: player})
				case source == "join intent" && strings.HasPrefix(statement, "SELECT owner_account,owner_revision FROM match_join_intent"):
					return sqlTraceRow(predecessorAccount, uint64(6))
				}
				return nil
			}
			require.NoError(t, s.CancelIntent(sqlTraceActor(t, player), player, "", request))
			require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
			for _, statement := range recorder.recorded() {
				require.False(t, strings.HasPrefix(statement, "UPDATE match_authority_ticket") || strings.HasPrefix(statement, "INSERT INTO match_join_intent"),
					"a predecessor's original must stay untouched: %s", statement)
			}
		})
	}
	t.Run("stale session", func(t *testing.T) {
		s := sqlTraceStore(t, newSQLTraceRecorder())
		s.roleGuard = rejectingRoleGuard
		err := s.CancelIntent(sqlTraceActor(t, player), player, "", request)
		require.ErrorIs(t, err, ErrOriginalPlayer)
		require.NotErrorIs(t, err, ErrForeignOriginal)
	})
}
