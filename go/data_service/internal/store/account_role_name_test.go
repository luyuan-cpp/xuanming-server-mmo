package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 「预留 → 绑定」状态机的目录一侧编排(docs/design/guild-phase2/03-names.md §3.4a):绑定排在提交点之后、先复核角色行;
// 绑定失败不改建角结局,重放补上;删角之后只给墓碑释放名字。目录各步用 flowSteps,名字登记表用下面的内存替身,
// 两边的调用记在同一条记录里,才看得出先后。真库上的对应用例见 account_role_name_tidb_test.go。

// fakeRoleNames 是 roleNameRegistry 的内存替身,语义抄 PlayerNameStore:名字行在别人名下回 Taken,已绑定回 AlreadyBound。
type fakeRoleNames struct {
	steps *flowSteps
	// rows 是名字行:name_norm → 登记在谁名下;bound 是其中已绑定的;restoredAt 是补登记写下的那一刻(created_ms = bound_ms)。
	rows       map[string]uint64
	bound      map[string]bool
	restoredAt map[string]uint64
	released   []string
	undone     []string
}

func newFakeRoleNames(steps *flowSteps) *fakeRoleNames {
	return &fakeRoleNames{steps: steps, rows: map[string]uint64{}, bound: map[string]bool{}, restoredAt: map[string]uint64{}}
}

func (f *fakeRoleNames) Bind(_ context.Context, player uint64, _, norm string, nowMs uint64) (BindOutcome, uint64, error) {
	if err := f.steps.step("bindName"); err != nil {
		return 0, 0, err
	}
	holder, held := f.rows[norm]
	switch {
	case held && holder != player:
		return BindTaken, holder, nil
	case held && f.bound[norm]:
		return BindAlreadyBound, 0, nil
	case held:
		f.bound[norm] = true
		return BindBound, 0, nil
	}
	f.rows[norm], f.bound[norm], f.restoredAt[norm] = player, true, nowMs
	return BindRestored, 0, nil
}

// UndoRestore 语义抄 PlayerNameStore:只删这名角色、本次补登记写下的那一行。
func (f *fakeRoleNames) UndoRestore(_ context.Context, player uint64, norm string, restoredMs uint64) (ReleaseOutcome, error) {
	if err := f.steps.step("undoRestore"); err != nil {
		return 0, err
	}
	f.undone = append(f.undone, norm)
	if holder, held := f.rows[norm]; held && holder == player && f.restoredAt[norm] == restoredMs {
		delete(f.rows, norm)
		delete(f.bound, norm)
		delete(f.restoredAt, norm)
		return ReleaseDeleted, nil
	}
	return ReleaseAbsent, nil
}

func (f *fakeRoleNames) BoundTo(_ context.Context, player uint64) (bool, error) {
	if err := f.steps.step("boundTo"); err != nil {
		return false, err
	}
	for norm, holder := range f.rows {
		if holder == player && f.bound[norm] {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRoleNames) ReleaseRemoved(_ context.Context, player uint64, norm string) (ReleaseOutcome, error) {
	if err := f.steps.step("releaseRemovedName"); err != nil {
		return 0, err
	}
	f.released = append(f.released, norm)
	if holder, held := f.rows[norm]; held && holder == player {
		delete(f.rows, norm)
		delete(f.bound, norm)
		return ReleaseDeleted, nil
	}
	return ReleaseAbsent, nil
}

// namedFlowStore 是接上名字替身的目录。routed 摆的角色名字是 "flow",这里先按登记(预留态)摆好名字行。
func namedFlowStore(t *testing.T) (*AccountDirectoryStore, *flowSteps, *fakeRoleNames) {
	t.Helper()
	s, steps := newFlowStore(2)
	names := newFakeRoleNames(steps)
	names.rows["flow"] = 11
	s.names = names
	return s, steps, names
}

// 绑定排在提交点之后、ref 激活之前,而且绑定之前在玩家片重读一次角色行(复核确已提交)。
func TestCreateRoleBindsTheNameRightAfterTheCommitPoint(t *testing.T) {
	s, steps, names := namedFlowStore(t)
	out, err := s.Create(context.Background(), "owner", steps.routed(11))
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
	require.Equal(t, []string{"readOwner", "commitRole", "readOwner", "bindName", "activateRef"},
		steps.only("readOwner", "commitRole", "bindName", "activateRef"), "预读 → 提交 → 复核 → 绑定 → 激活 ref")
	require.True(t, names.bound["flow"])
	require.EqualValues(t, 11, names.rows["flow"])
}

// 提交点之前失败、提交结局未知都不绑定:角色可能没建成,把预留钉成已绑定就再也释放不了。
func TestCreateRoleDoesNotBindBeforeTheCommitIsKnown(t *testing.T) {
	for _, step := range []string{"writeInitialRows", "commitRole"} {
		t.Run(step, func(t *testing.T) {
			s, steps, names := namedFlowStore(t)
			steps.fail[step] = []error{errFlow}
			_, err := s.Create(context.Background(), "owner", steps.routed(11))
			require.ErrorIs(t, err, errFlow)
			require.Zero(t, steps.count("bindName"))
			require.False(t, names.bound["flow"], "名字行还是预留态")
		})
	}
}

// 用例「建角提交后名字绑定失败可由重放补上」:绑定失败不改建角结局(角色已提交,列表照常有);
// 同一建角重放走预读那条路补上绑定,不再走提交点、不再碰存档。
func TestCreateRoleBindFailureKeepsTheRoleAndReplayBinds(t *testing.T) {
	ctx := context.Background()
	s, steps, names := namedFlowStore(t)
	role := steps.routed(11)
	steps.fail["bindName"] = []error{errFlow}

	out, err := s.Create(ctx, "owner", role)
	require.NoError(t, err, "提交之后的绑定失败不能把建成的角色报成失败")
	require.Len(t, out.Players, 1)
	require.False(t, names.bound["flow"])
	require.Equal(t, refStateActive, steps.refs["owner"][11].State, "绑定失败不挡后面的步骤")

	out, err = s.Create(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
	require.True(t, names.bound["flow"], "重放补上了绑定")
	require.Equal(t, 1, steps.count("commitRole"), "提交之后的重放不再走提交点")
	require.Equal(t, 1, steps.initial[11], "提交之后的重放不写存档")
}

// 复核对不上就不绑:角色行不在、已是墓碑、归属换了、名字不是这个。名字登记表一次都不调。
func TestBindRefusesARoleThatIsNotTheCommittedOne(t *testing.T) {
	for name, setup := range map[string]func(*flowSteps){
		"角色行不在": func(*flowSteps) {},
		"已删除":   func(f *flowSteps) { f.setOwner(11, "", 2, 0) },
		"名字不是这个": func(f *flowSteps) {
			f.setOwner(11, "owner", 1, 0)
			f.owners[11].Role.Name = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, steps, names := namedFlowStore(t)
			setup(steps)
			require.Error(t, s.bindRoleName(context.Background(), steps.routed(11)))
			require.Zero(t, steps.count("bindName"))
			require.False(t, names.bound["flow"])
		})
	}
}

// 没有名字的角色(早于名字功能的建角路径)没有可绑的;名字登记表没接上时同样跳过,建角照常。
func TestCreateRoleWithoutNameOrRegistrySkipsBinding(t *testing.T) {
	s, steps, _ := namedFlowStore(t)
	role := steps.routed(11)
	role.Name = ""
	_, err := s.Create(context.Background(), "owner", role)
	require.NoError(t, err)
	require.Zero(t, steps.count("bindName"))

	plain, plainSteps := newFlowStore(2)
	_, err = plain.Create(context.Background(), "owner", plainSteps.routed(11))
	require.NoError(t, err)
}

// 名字别人占着(建成的角色和别人共用一个名字):绑定回 Taken,什么都不改,建角结局不变,留给核账报告。
func TestCreateRoleBindTakenLeavesTheOtherHolder(t *testing.T) {
	s, steps, names := namedFlowStore(t)
	names.rows["flow"], names.bound["flow"] = 900, true
	_, err := s.Create(context.Background(), "owner", steps.routed(11))
	require.NoError(t, err)
	require.EqualValues(t, 900, names.rows["flow"])
	require.Equal(t, 1, steps.count("bindName"))
}

// 删角之后只给墓碑释放名字,按墓碑里留着的原名;在役的角色绝不释放,墓碑里没有名字的(判陈旧、封堵写的)不调。
func TestReleaseRemovedRoleNameOnlyForTombstones(t *testing.T) {
	ctx := context.Background()

	s, steps, names := namedFlowStore(t)
	steps.setOwner(11, "", 2, 0)
	names.bound["flow"] = true
	require.NoError(t, s.releaseRemovedRoleName(ctx, 11))
	require.Equal(t, []string{"flow"}, names.released)
	require.NotContains(t, names.rows, "flow", "墓碑之后名字释放了")

	s, steps, names = namedFlowStore(t)
	steps.setOwner(11, "owner", 1, 0)
	require.Error(t, s.releaseRemovedRoleName(ctx, 11), "在役角色的名字绝不释放")
	require.Empty(t, names.released)
	require.EqualValues(t, 11, names.rows["flow"])

	s, steps, names = namedFlowStore(t)
	steps.setOwner(11, "", 1, 0)
	steps.owners[11].Role.Name = ""
	require.NoError(t, s.releaseRemovedRoleName(ctx, 11))
	require.Empty(t, names.released, "墓碑里没有名字的不调")

	s, steps, names = namedFlowStore(t)
	steps.setOwner(11, "", 2, 0)
	steps.fail["releaseRemovedName"] = []error{errFlow}
	require.ErrorIs(t, s.releaseRemovedRoleName(ctx, 11), errFlow, "释放失败回错误:小表那一行留着,后台接着放")
}

// 用例「补绑撞上并发删角」(§3.4a):复核角色在役之后、补登记之前角色被删了,删角收尾放名字时这一行还不在。
// 补登记之后复读到墓碑,撤掉本次写的那一行,名字不会绑在墓碑上没人放。角色还在役时补登记留着;复读失败什么都不撤。
func TestBindRestoreRacingARemovalIsUndone(t *testing.T) {
	ctx := context.Background()

	s, steps, names := namedFlowStore(t)
	delete(names.rows, "flow") // 名字行在提交之前就没了:绑定要补登记
	steps.setOwner(11, "owner", 1, 0)
	steps.before["bindName"] = func() { steps.setOwner(11, "", 2, 0) } // 复核之后、补登记之前删了角(收尾时名字行还不在)
	require.Error(t, s.bindRoleName(ctx, steps.routed(11)), "撤销要进日志")
	require.NotContains(t, names.rows, "flow", "补登记撤掉了")
	require.Equal(t, []string{"flow"}, names.undone)
	require.Equal(t, []string{"readOwner", "bindName", "readOwner", "undoRestore"}, steps.only("readOwner", "bindName", "undoRestore"),
		"补登记之后再复读一次角色行")

	s, steps, names = namedFlowStore(t)
	delete(names.rows, "flow")
	steps.setOwner(11, "owner", 1, 0)
	require.NoError(t, s.bindRoleName(ctx, steps.routed(11)))
	require.EqualValues(t, 11, names.rows["flow"], "角色还在役:补登记留着,之后的删角收尾排在墓碑之后,看得到它")
	require.True(t, names.bound["flow"])
	require.Empty(t, names.undone)

	s, steps, names = namedFlowStore(t)
	delete(names.rows, "flow")
	steps.setOwner(11, "owner", 1, 0)
	steps.fail["readOwner"] = []error{nil, errFlow}
	require.ErrorIs(t, s.bindRoleName(ctx, steps.routed(11)), errFlow, "复读失败回错误进日志")
	require.EqualValues(t, 11, names.rows["flow"], "没有证据就不删")
	require.Empty(t, names.undone)

	// 名字行本来就在:不是补登记,绑定之后不复读。
	s, steps, names = namedFlowStore(t)
	steps.setOwner(11, "owner", 1, 0)
	require.NoError(t, s.bindRoleName(ctx, steps.routed(11)))
	require.Equal(t, 1, steps.count("readOwner"))
	require.Empty(t, names.undone)
}
