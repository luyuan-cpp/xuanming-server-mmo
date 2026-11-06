package data

import (
	"context"
	"database/sql"
	"testing"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 2026-11-05 修复（r2-guild#1 评审）回归：launch 恢复器的到期认领（真 TiDB，GUILD_ROLE_TEST_DSN）。
//   - 刚 Prepare、开局窗口还没过的原件不到期：任何副本都认领不到它，取消也就不会先于在途 Start 到达 match；
//   - 到期的原件只被一个副本认领（单行 CAS），认领后检查时刻按退避推后；state=2 与 state=1 同一调度，已终结的不再认领；
//   - 解不开的行同样被认领、单独报出、推到退避上限，排在它后面的原件照常轮到。
// 2026-11-05 复审追加：
//   - 实时路径拿到 match 的确定答复（业务拒绝）而没开成：ExpediteTrialLaunchCheck 让原件立即到期；没调它（Start 超时，
//     结局未知）的原件仍等开局窗口 + 宽限；它只对从未认领的 state1 原件生效、只往前提；
//   - 认领 CAS 的两个条件各自生效：拿着过期观察值的副本影响 0 行；读到之后已终结的原件影响 0 行。
// 时间一律取 SQL 时钟；用例不 sleep，「时间走过了检查时刻」用直接把 next_check_ms 改成 1 表示。

type trialLaunchSchedule struct {
	nextCheckMs uint64
	attempts    uint32
}

func readTrialLaunchSchedule(t *testing.T, ctx context.Context, db *sql.DB, requestID uint64) trialLaunchSchedule {
	t.Helper()
	var out trialLaunchSchedule
	require.NoError(t, db.QueryRowContext(ctx, "SELECT next_check_ms,check_attempts FROM guild_trial_launch WHERE request_id=?", requestID).
		Scan(&out.nextCheckMs, &out.attempts))
	return out
}

func trialSQLNowForTest(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var now uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT "+trialSQLNowMs).Scan(&now))
	return now
}

func makeTrialLaunchDue(t *testing.T, ctx context.Context, db *sql.DB, requestID uint64) {
	t.Helper()
	_, err := db.ExecContext(ctx, "UPDATE guild_trial_launch SET next_check_ms=1 WHERE request_id=?", requestID)
	require.NoError(t, err)
}

// prepareTrialLaunchForClaim 建一个全员同意的大厅并 Prepare 一份 launch 原件(state=1)。
func prepareTrialLaunchForClaim(t *testing.T, requestID, battleID uint64) (context.Context, *sql.DB, *ActivityRepo, TrialBattleKey, *TrialLobby) {
	t.Helper()
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store := consentedSQLLobby(t, actor, repo, requestID, 44, 3, 11, 22)
	lobby, err := store.Load(ctx, requestID)
	require.NoError(t, err)
	roster, err := store.LoadConsents(ctx, lobby)
	require.NoError(t, err)
	battle := actTrialKey(actTrialRow(), battleID, 44, 11, testNowMs)
	battle.LaunchRequestID = requestID
	_, err = activities.PrepareTrialLaunch(actor, requestID, battle, 1, roster, testNowMs)
	require.NoError(t, err)
	return ctx, db, activities, battle, lobby
}

func TestTiDBTrialLaunchClaimWaitsForLaunchWindowAndClaimsOnce(t *testing.T) {
	const requestID, battleID = 391, 910
	ctx, db, activities, battle, lobby := prepareTrialLaunchForClaim(t, requestID, battleID)
	_, prepared, err := activities.TrialLaunch(ctx, requestID)
	require.NoError(t, err)

	// 首次检查时刻落在开局许可窗口之后,与开局截止同为 SQL 时钟。
	first := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.GreaterOrEqual(t, first.nextCheckMs, lobby.LaunchDeadlineMs+uint64(TrialRecoveryGrace.Milliseconds()))
	require.Zero(t, first.attempts)
	require.Less(t, trialSQLNowForTest(t, ctx, db), first.nextCheckMs, "前置:用例此刻仍在开局窗口 + 宽限之内")

	// 窗口内:Start 可能还在路上,任何副本都认领不到它,也就不会重放取消。
	due, err := activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Empty(t, due.Originals)
	require.Empty(t, due.Undecodable)
	require.False(t, due.More)
	require.Equal(t, first, readTrialLaunchSchedule(t, ctx, db, requestID), "没认领就不改调度")

	// 过了检查时刻:恰好一个副本认领到,检查时刻按第 0 次的退避推后;另一个副本紧接着再认领,影响 0 行。
	makeTrialLaunchDue(t, ctx, db, requestID)
	before := trialSQLNowForTest(t, ctx, db)
	due, err = activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Len(t, due.Originals, 1)
	require.True(t, proto.Equal(prepared, due.Originals[0]))
	after := trialSQLNowForTest(t, ctx, db)
	claimed := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.EqualValues(t, 1, claimed.attempts)
	require.GreaterOrEqual(t, claimed.nextCheckMs, before+uint64(trialRecheckDelay(0, 0).Milliseconds()))
	require.LessOrEqual(t, claimed.nextCheckMs, after+uint64(trialRecheckDelay(0, 1).Milliseconds()))
	other, err := activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Empty(t, other.Originals, "同一条同一时刻只有一个副本在查")

	// state=2(实时路径登记了对局):登记不改调度;到期同样被认领,不受开局窗口约束,退避按认领次数加长。
	require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
	require.Equal(t, claimed, readTrialLaunchSchedule(t, ctx, db, requestID))
	makeTrialLaunchDue(t, ctx, db, requestID)
	before = trialSQLNowForTest(t, ctx, db)
	due, err = activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Len(t, due.Originals, 1)
	again := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.EqualValues(t, 2, again.attempts)
	require.GreaterOrEqual(t, again.nextCheckMs, before+uint64(trialRecheckDelay(1, 0).Milliseconds()))

	// 终结之后不再认领:恢复器只看 state 1/2。
	abort := &matchpb.CancelActivityBattleResponse{BattleId: battleID, FormationState: 5,
		TerminalDecision: matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, RequestDigest: "fixture-verifier-covered-separately"}
	require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, abort, testNowMs+1))
	makeTrialLaunchDue(t, ctx, db, requestID)
	due, err = activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Empty(t, due.Originals)
}

func TestTiDBTrialLaunchClaimSkipsUndecodableRow(t *testing.T) {
	const requestID, battleID, badID = 395, 915, 7
	ctx, db, activities, _, _ := prepareTrialLaunchForClaim(t, requestID, battleID)
	// 一行摘要对不上的原件(手工误改,或日后收紧解码规则后的历史行)。request_id 更小、检查时刻相同,排在到期队列队头。
	badOriginal := []byte{1, 2, 3}
	_, err := db.ExecContext(ctx, "INSERT INTO guild_trial_launch("+trialLaunchColumns+",next_check_ms) VALUES(?,0,1,?,?,?,?,?,1)",
		badID, badOriginal, []byte("not-a-sha256-digest"), testNowMs, testNowMs, []byte{})
	require.NoError(t, err)
	makeTrialLaunchDue(t, ctx, db, requestID)

	// 一页只看一行:队头的坏行被认领、单独报出,不报错、不整页失败。
	before := trialSQLNowForTest(t, ctx, db)
	due, err := activities.ClaimDueTrialLaunches(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{badID}, due.Undecodable)
	require.Empty(t, due.Originals)
	require.True(t, due.More)

	// 下一页就轮到排在它后面的正常原件:坏行不再卡住队列。
	due, err = activities.ClaimDueTrialLaunches(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, due.Undecodable)
	require.Len(t, due.Originals, 1)
	require.EqualValues(t, requestID, due.Originals[0].RequestId)

	// 坏行原样保留、等人工处理,只是检查时刻推到退避上限。
	var state uint32
	var raw []byte
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state,original_proto FROM guild_trial_launch WHERE request_id=?", badID).Scan(&state, &raw))
	require.EqualValues(t, 1, state)
	require.Equal(t, badOriginal, raw)
	bad := readTrialLaunchSchedule(t, ctx, db, badID)
	require.EqualValues(t, 1, bad.attempts)
	require.GreaterOrEqual(t, bad.nextCheckMs, before+uint64(trialRecheckCap.Milliseconds()))
}

// 实时路径拿到 match 的确定答复(业务拒绝,或预算不足没发 Start)而没开成:这次 Start 已不在途,ExpediteTrialLaunchCheck 把
// 首次检查提前到 SQL 现在,下一次认领就轮到它 —— 否则名单上的人要多挂约 35s。Start 超时(结局未知)时实时路径不调它,
// 原件仍等开局窗口 + 宽限。它只改调度:只对从未认领、仍是 state1 且没有 battle 的原件生效,只往前提,不往后推。
func TestTiDBTrialLaunchExpediteMakesOnlyAnUnclaimedStartDue(t *testing.T) {
	const requestID, battleID = 397, 917
	ctx, db, activities, battle, _ := prepareTrialLaunchForClaim(t, requestID, battleID)

	// Start 超时(结局未知):不提前,开局窗口 + 宽限之内谁都认领不到它。
	prepared := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.Less(t, trialSQLNowForTest(t, ctx, db), prepared.nextCheckMs, "前置:用例此刻仍在开局窗口 + 宽限之内")
	due, err := activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Empty(t, due.Originals)

	// match 同步拒绝:提前到 SQL 现在,不计认领次数;下一次认领就交出它。
	before := trialSQLNowForTest(t, ctx, db)
	require.NoError(t, activities.ExpediteTrialLaunchCheck(ctx, requestID))
	after := trialSQLNowForTest(t, ctx, db)
	expedited := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.GreaterOrEqual(t, expedited.nextCheckMs, before)
	require.LessOrEqual(t, expedited.nextCheckMs, after)
	require.Zero(t, expedited.attempts)
	// 重复调用只往前提、不往后推:检查时刻已不晚于现在,影响 0 行。
	require.NoError(t, activities.ExpediteTrialLaunchCheck(ctx, requestID))
	require.Equal(t, expedited, readTrialLaunchSchedule(t, ctx, db, requestID))
	due, err = activities.ClaimDueTrialLaunches(ctx, 20)
	require.NoError(t, err)
	require.Len(t, due.Originals, 1)
	require.EqualValues(t, requestID, due.Originals[0].RequestId)

	// 认领之后再调不改调度:由退避接管。
	claimed := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.EqualValues(t, 1, claimed.attempts)
	require.NoError(t, activities.ExpediteTrialLaunchCheck(ctx, requestID))
	require.Equal(t, claimed, readTrialLaunchSchedule(t, ctx, db, requestID))

	// 登记了对局(state2)的原件不吃这一条,即使认领次数被清零:Start 已开局,不是「确定没开成」。
	_, err = db.ExecContext(ctx, "UPDATE guild_trial_launch SET check_attempts=0 WHERE request_id=?", requestID)
	require.NoError(t, err)
	require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
	registered := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.NoError(t, activities.ExpediteTrialLaunchCheck(ctx, requestID))
	require.Equal(t, registered, readTrialLaunchSchedule(t, ctx, db, requestID))

	// 没有这一行(Prepare 未提交):影响 0 行,不报错。
	require.NoError(t, activities.ExpediteTrialLaunchCheck(ctx, requestID+1000))
}

// 认领 CAS 的两个条件各自守住一件事。顺序调用 ClaimDueTrialLaunches 测不出来(第二次扫描已读不到这一行),
// 这里直接拿观察值调 claimTrialLaunch,等价于两个副本在 CAS 之前读到了同一行:
//   - next_check_ms=观察值:先 CAS 的认领到,拿着同一个(已过期)观察值的另一个副本影响 0 行;
//   - state IN(1,2):读到之后、CAS 之前这一条已被终结(终结不改调度列),CAS 影响 0 行,不会把终结的原件交出去重放。
func TestTiDBTrialLaunchClaimCASRejectsStaleObservations(t *testing.T) {
	const requestID, battleID = 398, 918
	ctx, db, activities, _, _ := prepareTrialLaunchForClaim(t, requestID, battleID)
	makeTrialLaunchDue(t, ctx, db, requestID)
	observed := readTrialLaunchSchedule(t, ctx, db, requestID)

	claimed, err := activities.claimTrialLaunch(ctx, requestID, observed.nextCheckMs+1, trialRecheckBase)
	require.NoError(t, err)
	require.False(t, claimed, "观察值对不上的 CAS 影响 0 行")
	require.Equal(t, observed, readTrialLaunchSchedule(t, ctx, db, requestID))

	claimed, err = activities.claimTrialLaunch(ctx, requestID, observed.nextCheckMs, trialRecheckBase)
	require.NoError(t, err)
	require.True(t, claimed, "第一个副本认领到")
	claimed, err = activities.claimTrialLaunch(ctx, requestID, observed.nextCheckMs, trialRecheckBase)
	require.NoError(t, err)
	require.False(t, claimed, "第二个副本拿着同一个已过期的观察值,认领不到")
	afterClaim := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.EqualValues(t, 1, afterClaim.attempts, "只有一次认领生效")

	// 读到之后被终结:用零 battle 的取消墓碑释放(检查时刻不变),再拿当前观察值 CAS。
	_, original, err := activities.TrialLaunch(ctx, requestID)
	require.NoError(t, err)
	tombstone := &matchpb.CancelActivityBattleResponse{RequestCancelled: true, RequestDigest: "fixture-verifier-covered-separately"}
	require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, original, tombstone, testNowMs+1))
	terminal := readTrialLaunchSchedule(t, ctx, db, requestID)
	require.Equal(t, afterClaim, terminal, "终结不改调度列,只有 state 条件能挡住这次 CAS")
	claimed, err = activities.claimTrialLaunch(ctx, requestID, terminal.nextCheckMs, trialRecheckBase)
	require.NoError(t, err)
	require.False(t, claimed, "已终结的原件不能被认领")
	require.Equal(t, terminal, readTrialLaunchSchedule(t, ctx, db, requestID))
}
