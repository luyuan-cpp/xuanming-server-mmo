package authority

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
)

// 战斗剧本存储的真 TiDB 用例(MATCH_IT_TIDB,与本包其它事务用例同一夹具;没配就跳过,MATCH_IT_REQUIRE_TIDB=1 时判红)。
// 覆盖:新建与份数上限、改写的 CAS、只认本人;审核的 CAS、通过前再校验、流水同事务;选用必须已通过、条目上限、
// 同键改写不占额度;改写不动通过版;删除连带清掉选用、流水保留;流水的保留期清理(只删到期行、按游标翻页、重复与交错执行幂等)。
// 末尾的 TestBattleScriptReviewPurgeRejectsNonPositiveRetention 不连库。

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

// seedBattleScriptReview 直接写一行审核流水,reviewed_ms 由用例指定(审核入口只会写库当前时刻)。
func seedBattleScriptReview(t *testing.T, s *Store, scriptID uint64, revision uint32, reviewedMs uint64) {
	t.Helper()
	_, err := s.DB.ExecContext(context.Background(), "INSERT INTO match_battle_script_review(script_id,revision,owner_player_id,approved,reviewer,reason,reviewed_ms,content) VALUES(?,?,?,?,?,?,?,?)",
		scriptID, revision, uint64(91003), true, "ops", "", reviewedMs, battleScriptBytes(t, "reviewed"))
	require.NoError(t, err)
}

func battleScriptReviewKept(t *testing.T, s *Store, scriptID uint64, revision uint32) bool {
	t.Helper()
	var rows int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM match_battle_script_review WHERE script_id=? AND revision=?", scriptID, revision).Scan(&rows))
	return rows == 1
}

// 只删过了保留期的流水,剧本行与未到期的流水不动;两个副本各持游标交错着删、再整轮重跑,都不报错也不多删。
func TestBattleScriptReviewRetentionPurgesOnlyExpiredRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const retention = 180 * 24 * time.Hour
	retentionMs := uint64(retention.Milliseconds())

	// 真实审过的一版:流水的 reviewed_ms 是库当前时刻,在保留期内。
	const player = uint64(91003)
	_, err := s.SubmitBattleScript(ctx, BattleScriptSubmit{Player: player, NewScriptID: 7201, Content: battleScriptBytes(t, "fresh"), MaxScripts: 4})
	require.NoError(t, err)
	require.NoError(t, s.ReviewBattleScript(ctx, BattleScriptReview{ScriptID: 7201, Revision: 1, Approve: true, Reviewer: "ops"}))

	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	old := stamp - retentionMs - 60_000
	seedBattleScriptReview(t, s, 7301, 1, old)
	seedBattleScriptReview(t, s, 7301, 2, old+1)
	seedBattleScriptReview(t, s, 7302, 1, old+2)
	// 离分界还差 1 分钟:清理读到的库时刻比 stamp 晚,但用例跑不满 1 分钟,仍在保留期内。
	seedBattleScriptReview(t, s, 7303, 1, stamp-retentionMs+60_000)
	oldest, err := s.OldestBattleScriptReviewMs(ctx)
	require.NoError(t, err)
	require.Equal(t, old, oldest, "逾期告警读的是表里最老一行的审核时刻")

	// 副本 A 删一页(一行),游标停在 (old, 7301, 1);副本 B 从头把剩下到期的删完。
	cursorA, deleted, err := s.PurgeExpiredBattleScriptReviews(ctx, retention, BattleScriptReviewCursor{}, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.Equal(t, BattleScriptReviewCursor{ReviewedMs: old, ScriptID: 7301, Revision: 1}, cursorA, "满一页:游标停在这页最后一行")
	cursorB, deleted, err := s.PurgeExpiredBattleScriptReviews(ctx, retention, BattleScriptReviewCursor{}, 100)
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted)
	require.Equal(t, BattleScriptReviewCursor{}, cursorB, "不满一页:下一轮从头扫")
	// 副本 A 接着它的游标删:后面的行已被 B 删掉,读到空页回到开头,不报错。
	cursorA, deleted, err = s.PurgeExpiredBattleScriptReviews(ctx, retention, cursorA, 1)
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.Equal(t, BattleScriptReviewCursor{}, cursorA)
	_, deleted, err = s.PurgeExpiredBattleScriptReviews(ctx, retention, BattleScriptReviewCursor{}, 100)
	require.NoError(t, err)
	require.Zero(t, deleted, "幂等")

	require.False(t, battleScriptReviewKept(t, s, 7301, 1))
	require.False(t, battleScriptReviewKept(t, s, 7301, 2))
	require.False(t, battleScriptReviewKept(t, s, 7302, 1))
	require.True(t, battleScriptReviewKept(t, s, 7303, 1), "还在保留期内")
	require.True(t, battleScriptReviewKept(t, s, 7201, 1), "刚审的那一版")
	oldest, err = s.OldestBattleScriptReviewMs(ctx)
	require.NoError(t, err)
	require.Equal(t, stamp-retentionMs+60_000, oldest, "到期的删完之后,最老一行是保留期内最早的那一行")
	scripts, err := s.BattleScriptsOf(ctx, player)
	require.NoError(t, err)
	require.Len(t, scripts, 1, "清理只碰流水,不碰剧本行")
}

// 翻页按 (reviewed_ms, script_id, revision) 走:同一时刻的几行按主键排,不重不漏;扫到末尾回到开头。
func TestBattleScriptReviewRetentionPagesByCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const retention = 180 * 24 * time.Hour
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	old := stamp - uint64(retention.Milliseconds()) - 60_000
	seedBattleScriptReview(t, s, 7402, 1, old)
	seedBattleScriptReview(t, s, 7401, 2, old+1) // 下面三行同一时刻,按 (script_id, revision) 排
	seedBattleScriptReview(t, s, 7401, 3, old+1)
	seedBattleScriptReview(t, s, 7403, 1, old+1)
	seedBattleScriptReview(t, s, 7400, 1, old+2) // script_id 最小,但时刻最晚,排在最后

	type round struct {
		next    BattleScriptReviewCursor
		deleted int64
	}
	var got []round
	cursor := BattleScriptReviewCursor{}
	for i := 0; i < 4; i++ {
		next, deleted, err := s.PurgeExpiredBattleScriptReviews(ctx, retention, cursor, 2)
		require.NoError(t, err)
		got = append(got, round{next, deleted})
		cursor = next
	}
	require.Equal(t, []round{
		{BattleScriptReviewCursor{ReviewedMs: old + 1, ScriptID: 7401, Revision: 2}, 2},
		{BattleScriptReviewCursor{ReviewedMs: old + 1, ScriptID: 7403, Revision: 1}, 2},
		{BattleScriptReviewCursor{}, 1},
		{BattleScriptReviewCursor{}, 0},
	}, got)
	var left int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_script_review").Scan(&left))
	require.Zero(t, left)
	oldest, err := s.OldestBattleScriptReviewMs(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, oldest, stamp, "空表回库当前时刻:没有比现在更老的行")
}

// 保留期不为正(装配错了)时不删、不动游标,也不碰库:否则等于删光全部流水。不连库。
func TestBattleScriptReviewPurgeRejectsNonPositiveRetention(t *testing.T) {
	s := &Store{} // 没有库:判定必须在任何读写之前
	after := BattleScriptReviewCursor{ReviewedMs: 1, ScriptID: 2, Revision: 3}
	for _, retention := range []time.Duration{0, -time.Hour} {
		next, deleted, err := s.PurgeExpiredBattleScriptReviews(context.Background(), retention, after, 10)
		require.Error(t, err)
		require.Equal(t, after, next)
		require.Zero(t, deleted)
	}
}
