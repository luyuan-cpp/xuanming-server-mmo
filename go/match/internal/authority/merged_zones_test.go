package authority

import (
	"context"
	"testing"

	"match/internal/catalog"
	"match/internal/scope"

	pb "proto/match"
	"shared/placement"

	"github.com/stretchr/testify/require"
)

// fixtureMergedZones 用真缓存给 Store 接一份固定映射:不 Start 就不起 goroutine。
func fixtureMergedZones(t *testing.T, s *Store, merges map[uint32]uint32) {
	t.Helper()
	cache, err := placement.NewMergedZoneCache(context.Background(), func(context.Context) (map[uint32]uint32, error) {
		return merges, nil
	}, placement.MergedZoneCacheOptions{})
	require.NoError(t, err)
	s.BindMergedZones(cache)
}

func TestMergedZonesWithoutACacheMeansNoMerges(t *testing.T) {
	var absent *Store
	require.Nil(t, absent.MergedZones())
	require.Nil(t, (&Store{}).MergedZones())
	require.Equal(t, uint32(5), (&Store{}).MergedZones().Final(5))
	require.Equal(t, []uint32{5}, (&Store{}).MergedZones().Members(5))

	s := &Store{}
	fixtureMergedZones(t, s, map[uint32]uint32{4: 5, 5: 6})
	require.Equal(t, uint32(6), s.MergedZones().Final(4))
	require.Equal(t, []uint32{4, 5, 6}, s.MergedZones().Members(5))
}

// 读映射要全局库:没配 schema 就是没装配,不能当成「没有合服」。
func TestLoadMergedZonesNeedsTheGlobalSchema(t *testing.T) {
	var absent *Store
	_, err := absent.LoadMergedZones(context.Background(), placement.MergedZoneCacheOptions{})
	require.Error(t, err)
	_, err = (&Store{}).LoadMergedZones(context.Background(), placement.MergedZoneCacheOptions{})
	require.Error(t, err)
}

// 启动门禁的真库口径:全局库里读不到 zone_merged_into(没有表或没有权限)就回错误,调用方拒绝启动。
func TestAuthorityTiDBLoadMergedZonesFailsWhenTheTableIsUnreadable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&s.roleSchema))
	cache, err := s.LoadMergedZones(ctx, placement.MergedZoneCacheOptions{})
	require.Error(t, err, "testStore 的库里没有 zone_merged_into")
	require.Nil(t, cache)
}

func TestZoneInSQL(t *testing.T) {
	clause, args := zoneInSQL("r.home_zone", []uint32{5})
	require.Equal(t, "r.home_zone IN(?)", clause, "单个成员和原来的等值条件一样走索引")
	require.Equal(t, []any{uint32(5)}, args)

	clause, args = zoneInSQL("r.home_zone", []uint32{4, 5, 6})
	require.Equal(t, "r.home_zone IN(?,?,?)", clause)
	require.Equal(t, []any{uint32(4), uint32(5), uint32(6)}, args)

	clause, args = zoneInSQL("r.home_zone", nil)
	require.Equal(t, "1=0", clause, "空集合不匹配任何区,不能退化成不过滤")
	require.Empty(t, args)
}

func TestTeamUnitHomeZoneResolvesMergedZones(t *testing.T) {
	zones, err := placement.NewMergedZones(map[uint32]uint32{5: 6})
	require.NoError(t, err)
	group := func(zoneIDs ...uint32) []*pb.MatchAuthorityTicketRecord {
		out := make([]*pb.MatchAuthorityTicketRecord, 0, len(zoneIDs))
		for index, zone := range zoneIDs {
			out = append(out, &pb.MatchAuthorityTicketRecord{PlayerId: uint64(index + 1), ZoneId: zone})
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		group []*pb.MatchAuthorityTicketRecord
		zones *placement.MergedZones
		want  uint32
	}{
		{name: "全员同一个原始区,给原始区", group: group(5, 5), zones: zones, want: 5},
		{name: "没合服:两个区是混合 unit", group: group(5, 6), zones: nil, want: 0},
		{name: "合服后:两个原始区给最终区", group: group(5, 6), zones: zones, want: 6},
		{name: "合服后:顺序无关", group: group(6, 5, 5), zones: zones, want: 6},
		{name: "没参与合服的区还是混合 unit", group: group(5, 9), zones: zones, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := TeamUnitHomeZone(tc.group, tc.zones)
			require.NoError(t, err)
			require.Equal(t, tc.want, home)
		})
	}
	_, err = TeamUnitHomeZone(group(5, 0), zones)
	require.ErrorIs(t, err, ErrTeamRoute, "缺原 home 是损坏,合服不改变这条")
	_, err = TeamUnitHomeZone(nil, zones)
	require.ErrorIs(t, err, ErrTeamRoute)
}

func TestErrTeamCrossZoneIsATeamRouteRejection(t *testing.T) {
	require.ErrorIs(t, ErrTeamCrossZone, ErrTeamRoute)
}

// 提交点复核的范围树:票上存原始区,按最终区算层级。提交点和撮合(logic.poolUnitInput)同口径。
func TestAdmitScopeResolvesMergedZones(t *testing.T) {
	c, err := catalog.New(catalog.Rows{ZoneGroups: []scope.Range{{GroupID: 1, ZoneBegin: 5, ZoneEnd: 5}, {GroupID: 2, ZoneBegin: 6, ZoneEnd: 7}}})
	require.NoError(t, err)
	s := &Store{}
	require.NoError(t, s.ConfigureMatchCatalog(c, scope.Schedule{GroupAfterMs: 600_000, GlobalAfterMs: 600_000}))
	now := uint64(1_000_000)
	pair := func(a, b uint32) []*pb.MatchAuthorityTicketRecord {
		return []*pb.MatchAuthorityTicketRecord{{PlayerId: 1, ZoneId: a, EnqueuedMs: now - 1000}, {PlayerId: 2, ZoneId: b, EnqueuedMs: now - 1000}}
	}

	_, admitted := s.admitScope(pair(5, 6), now)
	require.False(t, admitted, "没合服:两个区要等满放宽阈值")

	fixtureMergedZones(t, s, map[uint32]uint32{5: 6})
	level, admitted := s.admitScope(pair(5, 6), now)
	require.True(t, admitted, "合服后算同一个 zone")
	require.Equal(t, scope.LevelZone, level)

	_, admitted = s.admitScope(pair(6, 7), now)
	require.False(t, admitted, "合服不影响别的区:同大区也要等满阈值")

	// 撤销合服:两张票还是原始区,回到合服之前的结论。
	fixtureMergedZones(t, s, nil)
	_, admitted = s.admitScope(pair(5, 6), now)
	require.False(t, admitted)
}
