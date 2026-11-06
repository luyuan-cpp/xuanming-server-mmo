package team

import (
	"context"
	"testing"

	"match/internal/authority"

	"github.com/stretchr/testify/require"
)

// 整队维护与闲置队伍到期按片分工(authority/work_owner.go):不负责的片不读,维护事务交给负责的副本;
// 没绑归属判定(单测、成员表为空)时全部片都归本副本。
func TestTeamTiDBMaintenanceReadsOnlyOwnedShards(t *testing.T) {
	f := newNativeTeamFixture(t)
	ctx := context.Background()
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.queued(t, 1, 101)

	launches, err := f.owner.TeamLaunchesAfter(ctx, 2, authority.TeamLaunchCursor{})
	require.NoError(t, err)
	require.Len(t, launches, 1, "没绑归属:全部片都归本副本")
	clause, args := f.owner.TeamIdleWorkFilter()
	require.Equal(t, "1=1", clause)
	require.Empty(t, args)

	f.owner.BindWorkOwner(func(string) bool { return false })
	launches, err = f.owner.TeamLaunchesAfter(ctx, 2, authority.TeamLaunchCursor{})
	require.NoError(t, err)
	require.Empty(t, launches, "不负责的片不读")
	clause, _ = f.owner.TeamIdleWorkFilter()
	require.Equal(t, "1=0", clause)
}
