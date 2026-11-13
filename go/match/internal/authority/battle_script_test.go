package authority

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
)

// 战斗剧本存储的真 TiDB 用例(MATCH_IT_TIDB,与本包其它事务用例同一夹具;没配就跳过,MATCH_IT_REQUIRE_TIDB=1 时判红)。
// 覆盖:新建与份数上限、改写的 CAS、只认本人;审核的 CAS、通过前再校验、流水同事务;选用必须已通过、条目上限、
// 同键改写不占额度;改写不动通过版;删除连带清掉选用、流水保留。

func battleScriptBytes(t *testing.T, name string) []byte {
	t.Helper()
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.BattleScript{SchemaVersion: 1, Name: name})
	require.NoError(t, err)
	return content
}

func TestBattleScriptSubmitRevisionAndLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const player = uint64(91001)

	first, err := s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, NewScriptID: 7001, Content: battleScriptBytes(t, "a"), MaxScripts: 2})
	require.NoError(t, err)
	require.EqualValues(t, 1, first.GetRevision())
	require.EqualValues(t, reviewPending, first.GetReviewState())
	_, err = s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, NewScriptID: 7002, Content: battleScriptBytes(t, "b"), MaxScripts: 2})
	require.NoError(t, err)
	_, err = s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, NewScriptID: 7003, Content: battleScriptBytes(t, "c"), MaxScripts: 2})
	require.ErrorIs(t, err, ErrBattleScriptLimit)

	revised, err := s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, ScriptID: 7001, ExpectedRevision: 1, Content: battleScriptBytes(t, "a2"), MaxScripts: 2})
	require.NoError(t, err)
	require.EqualValues(t, 2, revised.GetRevision())
	_, err = s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, ScriptID: 7001, ExpectedRevision: 1, Content: battleScriptBytes(t, "stale"), MaxScripts: 2})
	require.ErrorIs(t, err, ErrBattleScriptRevision)
	_, err = s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: 91999, ScriptID: 7001, ExpectedRevision: 2, Content: battleScriptBytes(t, "x"), MaxScripts: 2})
	require.ErrorIs(t, err, ErrBattleScriptNotFound, "another player's script must look absent")

	scripts, err := s.BattleScriptsOf(ctx, player)
	require.NoError(t, err)
	require.Len(t, scripts, 2)
}

func TestBattleScriptReviewLoadoutAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const player = uint64(91002)
	playerOrigin := battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER
	officialOrigin := battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL

	_, err := s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, NewScriptID: 7101, Content: battleScriptBytes(t, "v1"), MaxScripts: 4})
	require.NoError(t, err)
	_, err = s.SetBattleScriptLoadout(ctx, BattleScriptChoice{Player: player, MatchMode: 5, Origin: playerOrigin, PlayerScriptID: 7101, MaxEntries: 2})
	require.ErrorIs(t, err, ErrBattleScriptNotApproved, "a never-approved script cannot be chosen")

	require.ErrorIs(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7101, Revision: 2, Approve: true, Reviewer: "ops"}), ErrBattleScriptReviewStale)
	require.ErrorIs(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7101, Revision: 1, Approve: true, Reviewer: "ops",
		Validate: func([]byte) error { return errors.New("no longer valid") }}), ErrBattleScriptReviewInvalid)
	require.NoError(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7101, Revision: 1, Approve: true, Reviewer: "ops"}))
	require.ErrorIs(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7101, Revision: 1, Approve: true, Reviewer: "ops"}), ErrBattleScriptReviewStale,
		"a revision is reviewed once")
	revision, content, err := s.ApprovedBattleScript(ctx, 7101, player)
	require.NoError(t, err)
	require.EqualValues(t, 1, revision)
	require.Equal(t, battleScriptBytes(t, "v1"), content)
	_, _, err = s.ApprovedBattleScript(ctx, 7101, 91999)
	require.ErrorIs(t, err, ErrBattleScriptNotFound)

	list, err := s.SetBattleScriptLoadout(ctx, BattleScriptChoice{Player: player, MatchMode: 5, Origin: playerOrigin, PlayerScriptID: 7101, MaxEntries: 2})
	require.NoError(t, err)
	require.Len(t, list, 1)
	list, err = s.SetBattleScriptLoadout(ctx, BattleScriptChoice{Player: player, MatchMode: 4, Origin: officialOrigin, OfficialScriptID: 1, MaxEntries: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	_, err = s.SetBattleScriptLoadout(ctx, BattleScriptChoice{Player: player, MatchMode: 3, Origin: officialOrigin, OfficialScriptID: 1, MaxEntries: 2})
	require.ErrorIs(t, err, ErrBattleScriptLoadoutFull)
	list, err = s.SetBattleScriptLoadout(ctx, BattleScriptChoice{Player: player, MatchMode: 4, Origin: playerOrigin, PlayerScriptID: 7101, MaxEntries: 2})
	require.NoError(t, err, "overwriting an existing key does not take a new slot")
	require.Len(t, list, 2)

	// 作者改写:新版本待审,对局仍用通过版
	_, err = s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, ScriptID: 7101, ExpectedRevision: 1, Content: battleScriptBytes(t, "v2"), MaxScripts: 4})
	require.NoError(t, err)
	revision, content, err = s.ApprovedBattleScript(ctx, 7101, player)
	require.NoError(t, err)
	require.EqualValues(t, 1, revision)
	require.Equal(t, battleScriptBytes(t, "v1"), content)

	require.NoError(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7101, Revision: 2, Approve: false, Reviewer: "ops", Reason: "规则互相矛盾"}))
	scripts, err := s.BattleScriptsOf(ctx, player)
	require.NoError(t, err)
	require.Len(t, scripts, 1)
	require.EqualValues(t, reviewRejected, scripts[0].GetReviewState())
	require.Equal(t, "规则互相矛盾", scripts[0].GetRejectReason())
	require.EqualValues(t, 1, scripts[0].GetApprovedRevision())

	var reviews int
	require.NoError(t, testInstall(t, s).QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_script_review WHERE script_id=?", 7101).Scan(&reviews))
	require.Equal(t, 2, reviews)

	require.ErrorIs(t, s.DeleteBattleScript(ctx, player, 7101, 1), ErrBattleScriptRevision)
	require.NoError(t, s.DeleteBattleScript(ctx, player, 7101, 2))
	loadout, err := s.BattleScriptLoadout(ctx, player)
	require.NoError(t, err)
	require.Empty(t, loadout, "deleting a script clears every loadout entry that points at it")
	require.NoError(t, testInstall(t, s).QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_script_review WHERE script_id=?", 7101).Scan(&reviews))
	require.Equal(t, 2, reviews, "review history is kept for audit")
}
