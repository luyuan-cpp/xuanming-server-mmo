package store

import (
	"context"
	"errors"
	"testing"

	base "proto/common/base"
	"shared/accountdirectory"

	"github.com/stretchr/testify/require"
)

// 建角残留核账 2026-11-12 补的几项(create_residual.go 文件头):补回 ref 超过角色上限算要人看、名字修完删名字缓存、
// 删角的名字还登记着(removed_role_name)、孤儿名字(orphan_name)。目录各步用 residualFake,真库对应用例在 create_residual_names_tidb_test.go。

// residualTombstone 摆一块墓碑:角色行在、账号为空,role_proto 里留着原名(删角写的墓碑);name 为空是证明、判陈旧写的墓碑。
func (f *residualFake) residualTombstone(player uint64, name string) *residualRole {
	r := &residualRole{PlayerID: player, OwnerRevision: 2, Routed: true,
		Role:  &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: name},
		Route: RouteRow{PlayerID: player, HomeZone: 1, StorageID: residualStorage, PlacementVersion: 1, Rev: 1}}
	f.roles[player] = r
	return r
}

// reserved 摆一条预留态的名字行(和名字副本),登记时刻 createdMs。
func (f *residualFake) reserved(player uint64, norm string, createdMs uint64) {
	f.names[norm], f.unbound[norm], f.nameCreated[norm] = player, true, createdMs
	f.owned[player] = norm
}

// 用例「补回 ref 之后超过角色上限要人看」:修好了(repaired),但原因 over_role_cap 让它算要人看的项,工具以 1 退出;
// 名字缓存没删掉的那一种同理。别的修好的、复核时已经不缺的不算。
func TestCreateResidualOverRoleCapNeedsAttention(t *testing.T) {
	f := newResidualFake()
	f.put(101, "acct", "")
	delete(f.refs, residualRefKey{account: "acct", player: 101})
	f.overCap[101] = true
	f.put(102, "acct", "")
	delete(f.refs, residualRefKey{account: "acct", player: 102})

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Repair: CreateResidualRepairs{Refs: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"101 ref_missing repaired over_role_cap", "102 ref_missing repaired"}, residualOutline(report.Findings))
	require.True(t, residualFindingOf(t, report.Findings, 101, ResidualRefMissing).NeedsAttention(), "超过角色上限要人定")
	require.False(t, residualFindingOf(t, report.Findings, 102, ResidualRefMissing).NeedsAttention())

	for _, tc := range []struct {
		f    CreateResidualFinding
		want bool
	}{
		{CreateResidualFinding{Result: ResidualRepaired, Reason: residualReasonNameCacheStale}, true},
		{CreateResidualFinding{Result: ResidualRepaired}, false},
		{CreateResidualFinding{Result: ResidualUnchanged}, false},
		{CreateResidualFinding{Result: ResidualPlanned}, true},
		{CreateResidualFinding{Result: ResidualRefused, Reason: "role_changed"}, true},
	} {
		require.Equal(t, tc.want, tc.f.NeedsAttention(), "%+v", tc.f)
	}
}

// 用例「名字修复之后马上查得到」:名字三项修好之后删这名玩家的名字缓存(正、负缓存同一个键);删不掉标 name_cache_not_invalidated,
// 算要人看。没接缓存时名字一项都不修(name_cache_unavailable),ref、初始行照常修。
func TestCreateResidualNameRepairsInvalidateTheNameCache(t *testing.T) {
	ctx := context.Background()
	f := newResidualFake()
	f.put(103, "acct", "Charlie")
	delete(f.names, "charlie")
	f.put(104, "acct", "Delta")
	f.unbound["delta"] = true
	a, cache := residualAuditorWithCacheForTest(f)
	report, err := a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"103 name_missing repaired", "104 name_unbound repaired"}, residualOutline(report.Findings))
	require.Equal(t, []uint64{103, 104}, cache.deleted, "补回、置绑定之后都删缓存")

	f = newResidualFake()
	f.put(105, "acct", "Echo")
	delete(f.names, "echo")
	a, cache = residualAuditorWithCacheForTest(f)
	broken := errors.New("redis down")
	cache.fail = []error{broken, broken, broken}
	report, err = a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"105 name_missing repaired name_cache_not_invalidated"}, residualOutline(report.Findings))
	require.True(t, report.Findings[0].NeedsAttention(), "库修好了、缓存没删:要人手工删")
	require.EqualValues(t, 105, f.names["echo"], "库里照样修好")

	f = newResidualFake()
	f.put(106, "acct", "Foxtrot")
	delete(f.names, "foxtrot")
	delete(f.refs, residualRefKey{account: "acct", player: 106})
	a = residualAuditorForTest(f)
	a.cache = nil
	report, err = a.Run(ctx, CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{"106 ref_missing repaired", "106 name_missing refused name_cache_unavailable"}, residualOutline(report.Findings))
	require.Equal(t, []string{"restoreRef"}, f.writes(), "没接缓存不修名字")
}

// 用例「名字片恢复点早于删角释放」(编排):墓碑也扫,名字行或名字副本还在它名下就报 removed_role_name;apply 先复核墓碑,
// 按 (player_id, 判重键) 释放,删名字缓存;重放什么都不剩。名字副本指着别的名字只报告;墓碑里没有名字、名字早已归了别人的不报。
func TestCreateResidualReleasesNamesOfRemovedRoles(t *testing.T) {
	ctx := context.Background()
	f := newResidualFake()
	f.residualTombstone(131, "Gone")
	f.names["gone"], f.owned[131] = 131, "gone" // 名字行(已绑定)和名字副本都回来了
	f.residualTombstone(132, "OwnerOnly")
	f.owned[132] = "owneronly" // 名字行已放,名字副本还在
	f.residualTombstone(133, "Other")
	f.owned[133] = "elsewhere" // 名字副本指着别的名字
	f.residualTombstone(134, "")
	f.residualTombstone(135, "Retaken")
	f.names["retaken"] = 900 // 放掉之后别人登记了
	a, cache := residualAuditorWithCacheForTest(f)

	report, err := a.Run(ctx, CreateResidualScan{})
	require.NoError(t, err)
	require.Equal(t, []string{
		"131 removed_role_name planned",
		"132 removed_role_name planned",
		"133 removed_role_name manual player_holds_other_name",
	}, residualOutline(report.Findings))
	require.Empty(t, f.writes(), "plan 不写")

	report, err = a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"131 removed_role_name repaired",
		"132 removed_role_name repaired",
		"133 removed_role_name manual player_holds_other_name",
	}, residualOutline(report.Findings))
	_, held := f.names["gone"]
	require.False(t, held, "名字行删了")
	require.Empty(t, f.owned[131])
	require.Empty(t, f.owned[132], "名字副本删了")
	require.Equal(t, "elsewhere", f.owned[133], "指着别的名字的一个字节都不动")
	require.EqualValues(t, 900, f.names["retaken"], "别人后来登记的不碰")
	require.Equal(t, []uint64{131, 132}, cache.deleted)

	report, err = a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"133 removed_role_name manual player_holds_other_name"}, residualOutline(report.Findings), "重放只剩要人看的")
}

// 修之前复核墓碑:扫完之后角色行变了(恢复点调整之后墓碑没了、或者换了),拒绝,一笔都不写。
func TestCreateResidualRemovedRoleNameRechecksTheTombstone(t *testing.T) {
	f := newResidualFake()
	f.residualTombstone(131, "Gone")
	f.names["gone"], f.owned[131] = 131, "gone"
	f.onLoad = func(player uint64) { f.roles[player].OwnerRevision = 3 }
	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"131 removed_role_name refused role_changed"}, residualOutline(report.Findings))
	require.Empty(t, f.writes())
}

// 用例「玩家片冻结期间回滚释放失败留下的孤儿名字」(编排):名字行扫描只报登记早于 ResidualOrphanNameMinAge 的预留态、而且没有活角色的;
// 活角色自己的(name_unbound)、带着这个名字的墓碑(removed_role_name)留给角色行扫描。有提交证据只报告。
// apply 先证明(sealOrphanRole)、再只删预留态,删名字缓存;重放只剩要人看的。
func TestCreateResidualSettlesOrphanNames(t *testing.T) {
	ctx := context.Background()
	f := newResidualFake()
	old := uint64(residualNow.Add(-2 * ResidualOrphanNameMinAge).UnixMilli())
	f.reserved(141, "unrouted", old) // 没有角色行、没有路由行
	f.reserved(142, "routed", old)   // 没有角色行、有路由行:证明写墓碑
	f.routedOnly[142] = true
	f.reserved(143, "played", old) // 没有角色行,存档进过场
	f.evidence[143] = committedEvidenceStoreSaved
	f.reserved(144, "sealed", old) // 不带名字的墓碑(证明写的)
	f.residualTombstone(144, "")
	f.reserved(145, "fresh", uint64(residualNow.UnixMilli())) // 刚登记:可能是在途的建角
	f.put(146, "acct", "Mine")                                 // 活角色自己的名字还是预留态:角色行扫描报 name_unbound
	f.unbound["mine"], f.nameCreated["mine"] = true, old
	f.put(147, "acct", "Live")
	f.reserved(147, "stray", old) // 活角色名下另有一条预留态:ID 复用事故
	f.residualTombstone(148, "Named")
	f.reserved(148, "named", old) // 带着这个名字的墓碑:角色行扫描报 removed_role_name
	a, cache := residualAuditorWithCacheForTest(f)

	report, err := a.Run(ctx, CreateResidualScan{Target: ResidualTargetNames})
	require.NoError(t, err)
	require.Equal(t, ResidualTargetNames, report.Target)
	require.Equal(t, []string{
		"143 orphan_name manual store_saved",
		"142 orphan_name planned",
		"144 orphan_name planned",
		"147 orphan_name manual live_role_holds_other_name",
		"141 orphan_name planned",
	}, residualOutline(report.Findings), "按判重键的顺序报")
	require.Empty(t, f.writes(), "plan 不写")
	require.True(t, report.Done)
	require.Equal(t, "unrouted", report.NextAfterName)

	report, err = a.Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"143 orphan_name manual store_saved",
		"142 orphan_name repaired",
		"144 orphan_name repaired",
		"147 orphan_name manual live_role_holds_other_name",
		"141 orphan_name repaired",
	}, residualOutline(report.Findings))
	for _, norm := range []string{"unrouted", "routed", "sealed"} {
		_, held := f.names[norm]
		require.False(t, held, "孤儿名字删了:%s", norm)
	}
	for _, norm := range []string{"played", "fresh", "mine", "stray", "named"} {
		_, held := f.names[norm]
		require.True(t, held, "不该动的都留着:%s", norm)
	}
	require.Equal(t, "", f.roles[142].Account, "有路由行的先写了墓碑")
	require.Nil(t, f.roles[141], "没有路由行的不写墓碑")
	require.ElementsMatch(t, []uint64{141, 142, 144}, cache.deleted)
	require.Equal(t, 3, f.count("sealOrphanRole"), "先证明再删")

	report, err = a.Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"143 orphan_name manual store_saved",
		"147 orphan_name manual live_role_holds_other_name",
	}, residualOutline(report.Findings), "重放只剩要人看的")
}

// 修之前复核:扫完之后角色建成了(活角色)拒绝;没有角色行的重新找一遍证据,有了就拒绝;证明的事务里读到在役角色也拒绝。都不删名字。
func TestCreateResidualOrphanNameRechecksBeforeReleasing(t *testing.T) {
	ctx := context.Background()
	old := uint64(residualNow.Add(-2 * ResidualOrphanNameMinAge).UnixMilli())

	f := newResidualFake()
	f.reserved(151, "late", old)
	f.onLoad = func(player uint64) {
		if f.calls[len(f.calls)-1] == "scanNames" {
			return // 扫描时还没有角色行
		}
		f.put(player, "acct", "Late")
	}
	report, err := residualAuditorForTest(f).Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"151 orphan_name refused role_committed"}, residualOutline(report.Findings))
	require.Empty(t, f.writes())

	f = newResidualFake()
	f.reserved(152, "found", old)
	firstEvidence := true
	f.onLoad = func(uint64) {
		if !firstEvidence {
			f.evidence[152] = committedEvidenceRefActive // 扫完之后才查到 ACTIVE 的 ref
		}
		firstEvidence = false
	}
	report, err = residualAuditorForTest(f).Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"152 orphan_name refused ref_active"}, residualOutline(report.Findings))
	require.Empty(t, f.writes())

	f = newResidualFake()
	f.reserved(153, "racing", old)
	f.routedOnly[153] = true
	steps := &sealCommitsFake{residualFake: f}
	report, err = residualAuditorForTest(steps).Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"153 orphan_name refused role_committed"}, residualOutline(report.Findings))
	require.Zero(t, f.count("releaseReservedName"), "证明读到在役角色就不删")
}

// sealCommitsFake 摆「复核之后、证明之前角色提交了」:证明的事务里读到在役角色。
type sealCommitsFake struct{ *residualFake }

func (s *sealCommitsFake) sealOrphanRole(ctx context.Context, player uint64) (RoleSeal, error) {
	if _, err := s.residualFake.sealOrphanRole(ctx, player); err != nil {
		return 0, err
	}
	return RoleSealCommitted, nil
}

// 名字行按判重键分页,游标是最后一行的判重键;角色行带名字游标、名字行带 player_id 游标、未知的扫描对象都在碰库之前拒。
func TestCreateResidualNameScanPagesAndRejectsMixedCursors(t *testing.T) {
	ctx := context.Background()
	f := newResidualFake()
	old := uint64(residualNow.Add(-2 * ResidualOrphanNameMinAge).UnixMilli())
	for player, norm := range map[uint64]string{161: "aa", 162: "bb", 163: "cc"} {
		f.reserved(player, norm, old)
	}
	a := residualAuditorForTest(f)
	report, err := a.Run(ctx, CreateResidualScan{Target: ResidualTargetNames, Batch: 2, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, 2, report.Scanned)
	require.Equal(t, "bb", report.NextAfterName)
	require.False(t, report.Done)
	require.Equal(t, []string{"161 orphan_name planned", "162 orphan_name planned"}, residualOutline(report.Findings))
	report, err = a.Run(ctx, CreateResidualScan{Target: ResidualTargetNames, AfterName: report.NextAfterName, Batch: 2})
	require.NoError(t, err)
	require.True(t, report.Done)
	require.Equal(t, []string{"163 orphan_name planned"}, residualOutline(report.Findings))

	f.calls = nil
	for _, scan := range []CreateResidualScan{
		{Target: ResidualTargetNames, After: 7},
		{AfterName: "aa"},
		{Target: CreateResidualTarget(9)},
	} {
		_, err := a.Run(ctx, scan)
		require.ErrorIs(t, err, accountdirectory.ErrInvalid, "%+v", scan)
	}
	require.Empty(t, f.calls)
}

// 名字行这一页的证据读失败:结论不报、游标不前进,回错误;重跑从这一页开始,不会跳过。
func TestCreateResidualNameScanLookupFailureDoesNotSkipThePage(t *testing.T) {
	f := newResidualFake()
	old := uint64(residualNow.Add(-2 * ResidualOrphanNameMinAge).UnixMilli())
	f.reserved(171, "aa", old)
	f.fail["loadRole"] = []error{errors.New("player shard down")}
	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Target: ResidualTargetNames, Repair: CreateResidualRepairs{Names: true}})
	require.Error(t, err)
	require.Empty(t, report.Findings)
	require.Empty(t, report.NextAfterName, "游标停在这一页之前")
	require.Empty(t, f.writes())
}
