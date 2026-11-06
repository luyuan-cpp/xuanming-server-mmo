package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	base "proto/common/base"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/dirshard"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 建角残留核账的编排:判据、只报告的项、修之前复核、重放、分页、失败不跳页。
//
// residualFake 是 residualSteps 的内存替身,语义抄 SQL 实现(ref 存在即不写、名字登记的占用与冲突、存档库有行就不写);
// 真库上的对应用例见 create_residual_tidb_test.go。

const residualStorage uint32 = 7

type residualFake struct {
	roles   map[uint64]*residualRole
	refs    map[residualRefKey]bool
	names   map[string]uint64 // name_norm → 登记在谁名下
	unbound map[string]bool   // name_norm → 名字行还是预留态;不在里面的登记都已绑定(完整建成的角色)
	owned   map[uint64]string // player → 名字副本里的 name_norm
	stores  map[uint64][]int32
	allowed map[uint32]bool
	// others 是不在 roles 里的名字占用者。
	others map[uint64]residualPlayer

	calls []string
	// fail 给每一步排错误,按调用顺序消费。nil 表示这一次放行。
	fail map[string][]error
	// onLoad 在每次 loadRole 之前调,用来摆「扫完之后、修之前」角色行的变化。
	onLoad func(player uint64)
}

func newResidualFake() *residualFake {
	return &residualFake{
		roles:   map[uint64]*residualRole{},
		refs:    map[residualRefKey]bool{},
		names:   map[string]uint64{},
		unbound: map[string]bool{},
		owned:   map[uint64]string{},
		stores:  map[uint64][]int32{},
		allowed: map[uint32]bool{residualStorage: true},
		others:  map[uint64]residualPlayer{},
		fail:    map[string][]error{},
	}
}

func residualAuditorForTest(steps residualSteps) *CreateResidualAuditor {
	return &CreateResidualAuditor{steps: steps, shards: 1, maxRoles: 5, now: func() time.Time { return time.UnixMilli(1_700_000_000_000) }}
}

func (f *residualFake) step(name string) error {
	f.calls = append(f.calls, name)
	if queue := f.fail[name]; len(queue) > 0 {
		f.fail[name] = queue[1:]
		return queue[0]
	}
	return nil
}

// writes 是调用记录里所有修复写。
func (f *residualFake) writes() []string {
	var out []string
	for _, call := range f.calls {
		switch call {
		case "restoreRef", "restoreName", "restoreInitialRows":
			out = append(out, call)
		}
	}
	return out
}

func (f *residualFake) count(name string) int {
	n := 0
	for _, call := range f.calls {
		if call == name {
			n++
		}
	}
	return n
}

// put 摆一名完整建成的角色:ref、名字、名字副本、初始行都在,建角时刻取 player 本身。用例再按需删掉一样。
func (f *residualFake) put(player uint64, account, name string) *residualRole {
	r := &residualRole{
		PlayerID: player, Account: account, OwnerRevision: 1, InitialCommit: true, Routed: true,
		Role:  &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: name},
		Route: RouteRow{PlayerID: player, HomeZone: 1, StorageID: residualStorage, PlacementVersion: 1, Rev: 1, CreatedAtMs: player},
	}
	f.roles[player] = r
	f.refs[residualRefKey{account: account, player: player}] = true
	if key, ok := residualNameKeyOf(name); ok {
		f.names[key.norm] = player
		f.owned[player] = key.norm
	}
	f.stores[player] = []int32{playerStoreOwnerKind, playerStoreProfileKind}
	return r
}

func (f *residualFake) snapshot(r *residualRole) residualRole {
	c := *r
	if r.Role != nil {
		c.Role = proto.Clone(r.Role).(*base.AccountSimplePlayer)
	}
	c.OwnedNorm = f.owned[r.PlayerID]
	return c
}

func (f *residualFake) scanRoles(_ context.Context, _ int, after uint64, limit int) ([]residualRole, error) {
	if err := f.step("scanRoles"); err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(f.roles))
	for id, r := range f.roles {
		if id > after && r.Account != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	out := []residualRole{}
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, f.snapshot(f.roles[id]))
	}
	return out, nil
}

func (f *residualFake) loadRole(_ context.Context, player uint64) (*residualRole, error) {
	if f.onLoad != nil {
		f.onLoad(player)
	}
	if err := f.step("loadRole"); err != nil {
		return nil, err
	}
	r := f.roles[player]
	if r == nil {
		return nil, nil
	}
	c := f.snapshot(r)
	return &c, nil
}

func (f *residualFake) presentRefs(_ context.Context, keys []residualRefKey) (map[residualRefKey]bool, error) {
	if err := f.step("presentRefs"); err != nil {
		return nil, err
	}
	out := map[residualRefKey]bool{}
	for _, key := range keys {
		if f.refs[key] {
			out[key] = true
		}
	}
	return out, nil
}

func (f *residualFake) nameHolders(_ context.Context, norms []string) (map[string]residualNameRow, error) {
	if err := f.step("nameHolders"); err != nil {
		return nil, err
	}
	out := map[string]residualNameRow{}
	for _, norm := range norms {
		if holder, ok := f.names[norm]; ok {
			out[norm] = residualNameRow{Holder: holder, Bound: !f.unbound[norm]}
		}
	}
	return out, nil
}

func (f *residualFake) storageAllowed(storage uint32) bool { return f.allowed[storage] }

func (f *residualFake) storeKinds(_ context.Context, _ uint32, players []uint64) (map[uint64][]int32, error) {
	if err := f.step("storeKinds"); err != nil {
		return nil, err
	}
	out := map[uint64][]int32{}
	for _, player := range players {
		if kinds := f.stores[player]; len(kinds) > 0 {
			out[player] = slices.Clone(kinds)
		}
	}
	return out, nil
}

func (f *residualFake) describePlayers(_ context.Context, players []uint64) (map[uint64]residualPlayer, error) {
	if err := f.step("describePlayers"); err != nil {
		return nil, err
	}
	out := map[uint64]residualPlayer{}
	for _, player := range players {
		if r := f.roles[player]; r != nil {
			out[player] = residualPlayer{Live: r.Account != "", CreatedAtMs: r.Route.CreatedAtMs}
		} else if other, ok := f.others[player]; ok {
			out[player] = other
		}
	}
	return out, nil
}

func (f *residualFake) restoreRef(_ context.Context, account string, player uint64, _ int) (refRestore, error) {
	if err := f.step("restoreRef"); err != nil {
		return 0, err
	}
	key := residualRefKey{account: account, player: player}
	if f.refs[key] {
		return refPresent, nil
	}
	f.refs[key] = true
	return refRestored, nil
}

func (f *residualFake) restoreName(_ context.Context, player uint64, _, norm string, _ uint64) (ReserveOutcome, uint64, error) {
	if err := f.step("restoreName"); err != nil {
		return 0, 0, err
	}
	if holder, ok := f.names[norm]; ok && holder != player {
		return ReserveTaken, holder, nil
	}
	if other := f.owned[player]; other != "" && other != norm && f.names[other] == player {
		return ReserveConflict, 0, nil
	}
	outcome := ReserveInserted
	if _, ok := f.names[norm]; ok {
		outcome = ReserveAlreadyOwned
	}
	// 登记好之后置为已绑定:和 SQL 实现的 Reserve + Bind 同一个结局。
	f.names[norm], f.owned[player] = player, norm
	delete(f.unbound, norm)
	return outcome, 0, nil
}

func (f *residualFake) restoreInitialRows(_ context.Context, _ uint32, role *base.AccountSimplePlayer) (storeRestore, error) {
	if err := f.step("restoreInitialRows"); err != nil {
		return 0, err
	}
	kinds := f.stores[role.GetPlayerId()]
	switch {
	case slices.Contains(kinds, playerStoreOwnerKind):
		return storePresent, nil
	case len(kinds) > 0:
		return storePartial, nil
	}
	f.stores[role.GetPlayerId()] = []int32{playerStoreOwnerKind, playerStoreProfileKind}
	return storeRestored, nil
}

// residualOutline 把结论压成「player kind result [reason]」,一项一行,顺序就是报告的顺序。
func residualOutline(findings []CreateResidualFinding) []string {
	out := []string{}
	for _, f := range findings {
		line := fmt.Sprintf("%d %s %s", f.PlayerID, f.Kind, f.Result)
		if f.Reason != "" {
			line += " " + f.Reason
		}
		out = append(out, line)
	}
	return out
}

func residualFindingOf(t *testing.T, findings []CreateResidualFinding, player uint64, kind CreateResidualKind) CreateResidualFinding {
	t.Helper()
	for _, f := range findings {
		if f.PlayerID == player && f.Kind == kind {
			return f
		}
	}
	t.Fatalf("没有 player=%d kind=%s 这一项: %v", player, kind, residualOutline(findings))
	return CreateResidualFinding{}
}

var residualRepairAll = CreateResidualRepairs{Refs: true, Names: true, InitialRows: true}

// plan 只报告:每种残留各一个判据,能修的标 planned,不能自动修的标 manual 并写原因,一笔写都不发。
func TestCreateResidualPlanReportsEachResidualAndWritesNothing(t *testing.T) {
	f := newResidualFake()
	f.put(101, "acct", "Alpha")
	f.put(102, "acct", "Bravo")
	delete(f.refs, residualRefKey{account: "acct", player: 102})
	f.put(103, "acct", "Charlie")
	delete(f.names, "charlie")
	f.put(104, "acct", "Delta")
	f.names["delta"] = 900
	f.others[900] = residualPlayer{Live: true, CreatedAtMs: 5000}
	f.put(105, "acct", "Echo")
	delete(f.stores, 105)
	f.put(106, "acct", "Foxtrot").InitialCommit = false
	delete(f.stores, 106)
	f.put(107, "acct", "Golf")
	f.stores[107] = []int32{playerStoreProfileKind}
	f.put(108, "acct", "Hotel").Busy = true
	delete(f.stores, 108)
	f.put(109, "acct", "India").Role = nil
	f.put(110, "acct", "Juliet").Route.StorageID = 9
	f.put(111, "acct", "Kilo").Routed = false
	f.put(112, "acct", "Lima")
	delete(f.owned, 112)
	f.put(113, "buyer", "Mike").OwnerRevision = 2
	delete(f.stores, 113)

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{})
	require.NoError(t, err)
	require.Equal(t, []string{
		"102 ref_missing planned",
		"103 name_missing planned",
		"104 name_taken manual name_held_by_other",
		"105 initial_rows_missing planned",
		"106 initial_rows_missing manual legacy_role",
		"107 owner_row_missing manual partial_store_rows",
		"108 initial_rows_missing manual role_busy",
		"109 role_row_corrupt manual corrupt_role_row",
		"110 storage_not_listed manual storage_not_listed",
		"111 route_missing manual no_route_or_placement",
		"112 name_index_stale planned",
		"113 initial_rows_missing manual transferred_role",
	}, residualOutline(report.Findings))
	require.Empty(t, f.writes(), "plan 不写")
	require.Zero(t, f.count("loadRole"), "plan 不进修复的复核")
	require.Equal(t, 13, report.Scanned)
	require.True(t, report.Done)
	require.EqualValues(t, 113, report.NextAfter)

	taken := residualFindingOf(t, report.Findings, 104, ResidualNameTaken)
	require.EqualValues(t, 900, taken.Holder)
	require.True(t, taken.HolderLive)
	require.EqualValues(t, 104, taken.Keeper, "本角色建得早,按「建角早者留名」该它留名")
	for _, finding := range report.Findings {
		require.True(t, finding.NeedsAttention())
	}
}

// apply 只修选中的种类;修完之后重放 plan 什么都不剩,再 apply 一遍也不再写。
func TestCreateResidualApplyRepairsSelectedKindsAndReplayIsClean(t *testing.T) {
	f := newResidualFake()
	f.put(101, "acct", "Alpha")
	f.put(102, "acct", "Bravo")
	delete(f.refs, residualRefKey{account: "acct", player: 102})
	f.put(103, "acct", "Charlie")
	delete(f.names, "charlie")
	f.put(105, "acct", "Echo")
	delete(f.stores, 105)
	f.put(112, "acct", "Lima")
	delete(f.owned, 112)
	a := residualAuditorForTest(f)
	ctx := context.Background()

	report, err := a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Refs: true}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"102 ref_missing repaired",
		"103 name_missing planned",
		"105 initial_rows_missing planned",
		"112 name_index_stale planned",
	}, residualOutline(report.Findings))
	require.Equal(t, []string{"restoreRef"}, f.writes(), "没选的种类一笔都不写")

	report, err = a.Run(ctx, CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{
		"103 name_missing repaired",
		"105 initial_rows_missing repaired",
		"112 name_index_stale repaired",
	}, residualOutline(report.Findings))
	require.True(t, f.refs[residualRefKey{account: "acct", player: 102}])
	require.EqualValues(t, 103, f.names["charlie"])
	require.Equal(t, "lima", f.owned[112])
	require.Contains(t, f.stores[105], playerStoreOwnerKind)

	writes := len(f.writes())
	for _, repairs := range []CreateResidualRepairs{{}, residualRepairAll} {
		report, err = a.Run(ctx, CreateResidualScan{Repair: repairs})
		require.NoError(t, err)
		require.Empty(t, report.Findings, "修过的不再报")
	}
	require.Len(t, f.writes(), writes, "重放不再写")
}

// 修之前重读角色行:扫完之后过了户、删了角,前提就不成立了,这一项拒绝、一笔都不写;没变的照常修。
func TestCreateResidualApplyRechecksTheRoleBeforeEachRepair(t *testing.T) {
	f := newResidualFake()
	f.put(102, "acct", "Bravo")
	delete(f.refs, residualRefKey{account: "acct", player: 102})
	f.put(103, "acct", "Charlie")
	delete(f.names, "charlie")
	f.put(105, "acct", "Echo")
	delete(f.stores, 105)
	f.onLoad = func(player uint64) {
		switch player {
		case 102:
			f.roles[102].Account = "buyer"
		case 105:
			f.roles[105].Account = ""
		}
	}

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{
		"102 ref_missing refused role_changed",
		"103 name_missing repaired",
		"105 initial_rows_missing refused role_changed",
	}, residualOutline(report.Findings))
	require.Equal(t, []string{"restoreName"}, f.writes())
}

// 名字已经登记在别的角色名下:只报告,任何一方都不动。裁决按「建角早者留名」,占用者不是活角色时名字该归本角色,判不出回 0。
func TestCreateResidualNameTakenIsReportedNeverOverwritten(t *testing.T) {
	f := newResidualFake()
	f.put(104, "acct", "Delta")
	f.names["delta"] = 900
	f.others[900] = residualPlayer{Live: true, CreatedAtMs: 5000}
	f.put(113, "acct", "Mike")
	f.names["mike"] = 901
	f.put(114, "acct", "November")
	f.names["november"] = 902
	f.others[902] = residualPlayer{Live: true, CreatedAtMs: 50}
	f.put(115, "acct", "Oscar")
	f.names["oscar"] = 903
	f.others[903] = residualPlayer{Live: true, CreatedAtMs: 115}
	names := map[string]uint64{}
	for norm, holder := range f.names {
		names[norm] = holder
	}

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{
		"104 name_taken manual name_held_by_other",
		"113 name_taken manual name_held_by_other",
		"114 name_taken manual name_held_by_other",
		"115 name_taken manual name_held_by_other",
	}, residualOutline(report.Findings))
	require.Empty(t, f.writes(), "名字被占不进修复")
	require.Equal(t, names, f.names, "名字登记一个字节都不改")

	for player, want := range map[uint64]struct {
		holder uint64
		live   bool
		keeper uint64
	}{
		104: {900, true, 104},
		113: {901, false, 113},
		114: {902, true, 902},
		115: {903, true, 0},
	} {
		got := residualFindingOf(t, report.Findings, player, ResidualNameTaken)
		require.Equal(t, want.holder, got.Holder, "player=%d", player)
		require.Equal(t, want.live, got.HolderLive, "player=%d", player)
		require.Equal(t, want.keeper, got.Keeper, "player=%d", player)
	}
}

// 建角提交之后名字绑定没做完:名字登记在本角色名下却还是预留态。plan 报 name_unbound,apply 选 name 时置为已绑定
// (名字副本缺的一并补上,只报一项),重放什么都不剩。名字是别人的预留态时照旧是 name_taken,不当成本角色的去绑。
func TestCreateResidualBindsCommittedRoleWhoseNameIsStillReserved(t *testing.T) {
	f := newResidualFake()
	f.put(101, "acct", "Alpha")
	f.put(102, "acct", "Bravo")
	f.unbound["bravo"] = true
	f.put(103, "acct", "Charlie")
	f.unbound["charlie"] = true
	delete(f.owned, 103)
	f.put(104, "acct", "Delta")
	f.names["delta"], f.unbound["delta"] = 900, true
	f.others[900] = residualPlayer{Live: false}
	a := residualAuditorForTest(f)
	ctx := context.Background()

	report, err := a.Run(ctx, CreateResidualScan{})
	require.NoError(t, err)
	require.Equal(t, []string{
		"102 name_unbound planned",
		"103 name_unbound planned",
		"104 name_taken manual name_held_by_other",
	}, residualOutline(report.Findings))
	require.Empty(t, f.writes(), "plan 不写")

	report, err = a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"102 name_unbound repaired",
		"103 name_unbound repaired",
		"104 name_taken manual name_held_by_other",
	}, residualOutline(report.Findings))
	require.False(t, f.unbound["bravo"] || f.unbound["charlie"], "本角色的名字都置为已绑定")
	require.True(t, f.unbound["delta"], "别人的预留态不动")
	require.EqualValues(t, 900, f.names["delta"])
	require.Equal(t, "charlie", f.owned[103], "名字副本一并补上")
	require.Equal(t, []string{"restoreName", "restoreName"}, f.writes())

	report, err = a.Run(ctx, CreateResidualScan{Repair: CreateResidualRepairs{Names: true}})
	require.NoError(t, err)
	require.Equal(t, []string{"104 name_taken manual name_held_by_other"}, residualOutline(report.Findings), "修过的不再报")
	require.Len(t, f.writes(), 2, "重放不再写")
}

// 挂着删角令牌的角色:名字的三种残留只报告(role_removing),不修。补回去的名字可能赶在删角收尾放名字之后,绑在墓碑上没人放。
// ref 照修:多出来的陈旧 ref 由读路径过滤、后台判陈旧清掉,坏不了任何东西;初始行照旧按 role_busy 只报告。
// 删角撤销之后(令牌没了)重跑,照常修。
func TestCreateResidualLeavesNamesOfARemovingRoleAlone(t *testing.T) {
	f := newResidualFake()
	removing := func(r *residualRole) { r.Busy, r.Removing = true, true }
	removing(f.put(121, "acct", "Romeo"))
	delete(f.names, "romeo")
	removing(f.put(122, "acct", "Sierra"))
	f.unbound["sierra"] = true
	removing(f.put(123, "acct", "Tango"))
	delete(f.owned, 123)
	removing(f.put(124, "acct", "Uniform"))
	delete(f.refs, residualRefKey{account: "acct", player: 124})
	delete(f.stores, 124)
	f.put(125, "acct", "Victor")
	delete(f.names, "victor")
	a := residualAuditorForTest(f)
	ctx := context.Background()

	report, err := a.Run(ctx, CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{
		"121 name_missing manual role_removing",
		"122 name_unbound manual role_removing",
		"123 name_index_stale manual role_removing",
		"124 ref_missing repaired",
		"124 initial_rows_missing manual role_busy",
		"125 name_missing repaired",
	}, residualOutline(report.Findings))
	require.Equal(t, []string{"restoreRef", "restoreName"}, f.writes(), "删角进行中的角色,名字一笔都不写")
	_, restored := f.names["romeo"]
	require.False(t, restored)

	for player := uint64(121); player <= 124; player++ {
		f.roles[player].Busy, f.roles[player].Removing = false, false
	}
	report, err = a.Run(ctx, CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err)
	require.Equal(t, []string{
		"121 name_missing repaired",
		"122 name_unbound repaired",
		"123 name_index_stale repaired",
		"124 initial_rows_missing repaired",
	}, residualOutline(report.Findings), "删角撤销之后照常修")
}

// 按页扫,游标是最后一名扫到的角色;限量扫完给出下一段的起点,接着扫不重不漏。
func TestCreateResidualPagesThroughTheShardAndResumes(t *testing.T) {
	f := newResidualFake()
	for player := uint64(201); player <= 205; player++ {
		f.put(player, "acct", "")
	}
	delete(f.refs, residualRefKey{account: "acct", player: 203})
	delete(f.refs, residualRefKey{account: "acct", player: 205})
	f.put(206, "", "") // 墓碑不扫
	a := residualAuditorForTest(f)
	ctx := context.Background()

	report, err := a.Run(ctx, CreateResidualScan{Batch: 2, Limit: 3})
	require.NoError(t, err)
	require.Equal(t, 3, report.Scanned)
	require.EqualValues(t, 203, report.NextAfter)
	require.False(t, report.Done)
	require.Equal(t, []string{"203 ref_missing planned"}, residualOutline(report.Findings))

	report, err = a.Run(ctx, CreateResidualScan{After: report.NextAfter, Batch: 2})
	require.NoError(t, err)
	require.Equal(t, 2, report.Scanned)
	require.EqualValues(t, 205, report.NextAfter)
	require.True(t, report.Done)
	require.Equal(t, []string{"205 ref_missing planned"}, residualOutline(report.Findings))
}

// 一页的证据读失败:这一页的结论不报,游标停在上一页末尾,重跑从这一页开始,不会跳过。
func TestCreateResidualLookupFailureDoesNotSkipThePage(t *testing.T) {
	f := newResidualFake()
	for player := uint64(301); player <= 304; player++ {
		f.put(player, "acct", "")
	}
	delete(f.refs, residualRefKey{account: "acct", player: 302})
	delete(f.refs, residualRefKey{account: "acct", player: 304})
	f.fail["presentRefs"] = []error{nil, errors.New("account shard down")}

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Batch: 2, Repair: residualRepairAll})
	require.Error(t, err)
	require.Equal(t, 2, report.Scanned)
	require.EqualValues(t, 302, report.NextAfter)
	require.False(t, report.Done)
	require.Equal(t, []string{"302 ref_missing repaired"}, residualOutline(report.Findings))
	require.Equal(t, []string{"restoreRef"}, f.writes(), "失败的那一页不修")
}

// 修复失败分开报:闸门冻结是确定没写,拒绝;库里冒出别的行回滚,拒绝;别的错误结局未知,标失败。
func TestCreateResidualRepairFailuresAreReportedPerItem(t *testing.T) {
	f := newResidualFake()
	f.put(401, "acct", "")
	delete(f.refs, residualRefKey{account: "acct", player: 401})
	f.put(402, "acct", "")
	delete(f.stores, 402)
	f.put(403, "acct", "")
	delete(f.stores, 403)
	f.put(404, "acct", "Papa")
	delete(f.names, "papa")
	f.fail["restoreRef"] = []error{fmt.Errorf("lock gate: %w", authoritysql.ErrWriterFrozen)}
	f.fail["restoreInitialRows"] = []error{errors.New("connection reset"), accountdirectory.ErrConflict}
	f.fail["restoreName"] = []error{authoritysql.ErrWriterFrozen}

	report, err := residualAuditorForTest(f).Run(context.Background(), CreateResidualScan{Repair: residualRepairAll})
	require.NoError(t, err, "单项失败不中断整片")
	require.Equal(t, []string{
		"401 ref_missing refused writer_gate_frozen",
		"402 initial_rows_missing failed",
		"403 initial_rows_missing refused store_changed",
		"404 name_missing refused writer_gate_frozen",
	}, residualOutline(report.Findings))
}

// 参数不对在碰库之前就拒。
func TestCreateResidualRejectsBadScansBeforeTouchingTheStore(t *testing.T) {
	f := newResidualFake()
	a := residualAuditorForTest(f)
	for _, scan := range []CreateResidualScan{{Slot: 1}, {Slot: -1}, {Batch: CreateResidualMaxBatch + 1}, {Batch: -1}, {Limit: -1}} {
		_, err := a.Run(context.Background(), scan)
		require.ErrorIs(t, err, accountdirectory.ErrInvalid, "%+v", scan)
	}
	require.Empty(t, f.calls)
}

// 名字修复持着两个库的闸门行锁再写:任何一个闸门冻结、或者锁闸门行失败,都不写。
func TestWithGatesHeldWritesOnlyWhileEveryGateIsOpen(t *testing.T) {
	open := func(connector fakeGateRowConnector) *sql.DB {
		db := sql.OpenDB(connector)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	first, second := open(fakeGateRowConnector{writable: 1}), open(fakeGateRowConnector{writable: 1})
	frozen := open(fakeGateRowConnector{writable: 0})
	cause := errors.New("connection lost")
	broken := open(fakeGateRowConnector{err: cause})
	ctx := context.Background()
	wrote := 0
	write := func() error {
		wrote++
		return nil
	}

	require.NoError(t, withGatesHeld(ctx, []*sql.DB{first, second}, write))
	require.Equal(t, 1, wrote)

	err := withGatesHeld(ctx, []*sql.DB{first, frozen}, write)
	require.True(t, authoritysql.IsWriterFrozen(err), "%v", err)
	require.Equal(t, 1, wrote, "有一个闸门冻结就不写")

	err = withGatesHeld(ctx, []*sql.DB{broken}, write)
	require.ErrorIs(t, err, cause)
	require.False(t, authoritysql.IsWriterFrozen(err))
	require.Equal(t, 1, wrote, "锁不住闸门行不能当成开着")
}

// 名字行在名字片,名字副本在玩家片:两片按片号升序各锁一次,同一片只锁一次。多个工具实例同时跑也按同一个顺序锁,不会互等成环。
func TestNameGatesLockEachShardOnceInSlotOrder(t *testing.T) {
	pools := make([]*sql.DB, shardFixtureK)
	for slot := range pools {
		pools[slot] = sql.OpenDB(fakeGateRowConnector{writable: 1})
		t.Cleanup(func() { _ = pools[slot].Close() })
	}
	st := &sqlResidualSteps{shards: &shardPools{k: shardFixtureK, names: make([]string, shardFixtureK), pools: pools}}
	slotsOf := func(dbs []*sql.DB) []int {
		out := []int{}
		for _, db := range dbs {
			out = append(out, slices.Index(pools, db))
		}
		return out
	}
	sawShared, sawSplit := false, false
	for _, norm := range []string{"yunzhongjun", "alpha", "bravo"} {
		for player := uint64(1); player <= 64; player++ {
			nameSlot, playerSlot := dirshard.SlotOfName(norm, shardFixtureK), dirshard.SlotOfPlayer(player, shardFixtureK)
			want := []int{nameSlot}
			if playerSlot != nameSlot {
				want = []int{min(nameSlot, playerSlot), max(nameSlot, playerSlot)}
				sawSplit = true
			} else {
				sawShared = true
			}
			require.Equal(t, want, slotsOf(st.nameGates(player, norm)), "player=%d norm=%s", player, norm)
		}
	}
	require.True(t, sawShared && sawSplit, "两种情形都要覆盖到")
}
