package authority

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "proto/match"
	"shared/rolefence/fencetest"
)

// 原生 SQL 回归(MATCH_IT_TIDB):前主人排过队,票到期结束,Join 意图没有撤销墓碑;之后角色过户。
// 同一台设备上的新主人带着前主人留下的请求号来查、来撤:查询回「不会再入队」,撤销回成功,前主人的原件一个字节不动;
// 新主人沿这个请求号入队被拒,用自己的新请求号照常入队。
func TestQueueTiDBPredecessorJoinRequestIsClosedForNewOwner(t *testing.T) {
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	const player = uint64(31)
	previous, ticket, profile := nativeRoleJoin(t, s, ctx, player)

	// 过户的前提是排队已经结束(RoleFencePending):这里让票到期。
	_, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET expires_ms=1 WHERE player_id=?", player)
	require.NoError(t, err)
	expired, err := s.Ticket(previous, player)
	require.NoError(t, err)
	require.Equal(t, uint32(4), expired.State)
	closed, err := s.QueueRequestClosed(previous, player, ticket.RequestId)
	require.NoError(t, err)
	require.False(t, closed, "前主人自己看:票到期不是撤销墓碑")
	before := nativeMatchSnapshot(t, s, ctx)

	fencetest.Update(t, ctx, s.DB, player, "buyer", 8, 0)
	buyer := nativeRoleActor(t, ctx, player, "buyer", 8)

	closed, err = s.QueueRequestClosed(buyer, player, ticket.RequestId)
	require.NoError(t, err)
	require.True(t, closed, "对新主人,前主人的请求号不会再入队")
	current, err := s.Ticket(buyer, player)
	require.NoError(t, err)
	require.Nil(t, current, "前主人的票不算新主人的")
	require.NoError(t, s.CancelIntent(buyer, player, ticket.Ticket, ticket.RequestId))
	require.Equal(t, before, nativeMatchSnapshot(t, s, ctx), "查询与撤销都不改前主人的原件")

	_, err = s.QueueRequestClosed(previous, player, ticket.RequestId)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	require.NotErrorIs(t, err, ErrForeignOriginal, "前主人的旧会话本身已失效")

	n, err := s.Now(ctx)
	require.NoError(t, err)
	require.NoError(t, s.SavePlan(buyer, player, profile, n+120000))
	offered := func(request string) *pb.MatchAuthorityTicketRecord {
		return &pb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: uuid.NewString(), RequestId: request, Mode: 3, ConfigId: 1, TeamSize: 2, RatingCenti: 150000}
	}
	_, _, err = s.Join(buyer, offered(ticket.RequestId), profile.PlanId, profile.SessionBinding)
	require.ErrorIs(t, err, ErrOriginalPlayer, "新主人不能沿前主人的请求号入队")
	queued, created, err := s.Join(buyer, offered(uuid.NewString()), profile.PlanId, profile.SessionBinding)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "buyer", queued.OwnerAccount)
	require.EqualValues(t, 8, queued.OwnerRevision)
}
