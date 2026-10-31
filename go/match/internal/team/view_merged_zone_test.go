package team

import (
	"context"
	"testing"

	"match/internal/authority"

	teampb "proto/team"
	"shared/placement"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// mergedZoneRecord 造一支 2 区队长的队伍:成员、申请人、被邀请人分别在源区 2、目标区 1、没参与合服的 9 区。
func mergedZoneRecord() *teampb.TeamRecord {
	rec := withInvite(withApplication(newRecord(1, 2), 3, 2, ruleNow-1000, ruleNow+60000), 4, 2, ruleNow-1000, ruleNow+60000)
	rec.ZoneId = 2
	rec.Members[0].ZoneId = 2
	rec.Members[1].ZoneId = 9
	return rec
}

// 视图给最终区,记录不动。nil 映射(回执里存的形态)原样给原始区。
func TestTeamViewGivesFinalZones(t *testing.T) {
	zones, err := placement.NewMergedZones(map[uint32]uint32{2: ruleZone})
	require.NoError(t, err)
	rec := mergedZoneRecord()
	before := proto.Clone(rec)

	view := teamViewFor(1, rec, 1, 1, ruleNow, nil, zones)
	require.Equal(t, ruleZone, view.GetZoneId())
	require.Equal(t, ruleZone, view.GetMembers()[0].GetZoneId())
	require.Equal(t, uint32(9), view.GetMembers()[1].GetZoneId(), "没参与合服的区不动")
	require.Equal(t, ruleZone, view.GetApplications()[0].GetPlayer().GetZoneId())
	require.Equal(t, ruleZone, view.GetPendingInvites()[0].GetInvitee().GetZoneId())
	require.True(t, proto.Equal(before, rec), "记录里还是原始区")

	raw := teamViewFor(1, rec, 1, 1, ruleNow, nil, nil)
	require.Equal(t, uint32(2), raw.GetZoneId())
	require.Equal(t, uint32(2), raw.GetMembers()[0].GetZoneId())

	invite := incomingInviteView(rec, 4, ruleNow, nil, zones)
	require.Equal(t, ruleZone, invite.GetZoneId())
	require.Equal(t, ruleZone, invite.GetInviter().GetZoneId())
	require.Equal(t, uint32(2), incomingInviteView(rec, 4, ruleNow, nil, nil).GetZoneId())
}

// 正常回包和重放都经 decorateSQLReplay:回执里存原始区,按当前映射解析,撤销合服之后回到原始区。
func TestDecorateSQLReplayResolvesStoredZones(t *testing.T) {
	h := newServiceHarness(t)
	store := &authority.Store{}
	h.svcCtx.Authority = store
	stored := func() *teampb.TeamResponse {
		return &teampb.TeamResponse{Team: teamViewFor(1, mergedZoneRecord(), 1, 1, ruleNow, nil, nil)}
	}
	bind := func(merges map[uint32]uint32) {
		cache, err := placement.NewMergedZoneCache(context.Background(), func(context.Context) (map[uint32]uint32, error) {
			return merges, nil
		}, placement.MergedZoneCacheOptions{})
		require.NoError(t, err)
		store.BindMergedZones(cache)
	}

	bind(map[uint32]uint32{2: ruleZone})
	merged := h.service.decorateSQLReplay(context.Background(), stored()).GetTeam()
	require.Equal(t, ruleZone, merged.GetZoneId())
	require.Equal(t, ruleZone, merged.GetMembers()[0].GetZoneId())
	require.Equal(t, uint32(9), merged.GetMembers()[1].GetZoneId())
	require.Equal(t, ruleZone, merged.GetApplications()[0].GetPlayer().GetZoneId())
	require.Equal(t, ruleZone, merged.GetPendingInvites()[0].GetInvitee().GetZoneId())

	bind(nil)
	unmerged := h.service.decorateSQLReplay(context.Background(), stored()).GetTeam()
	require.Equal(t, uint32(2), unmerged.GetZoneId())
	require.Equal(t, uint32(2), unmerged.GetMembers()[0].GetZoneId())
}
