package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	battlepb "proto/battle"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	dspb "proto/data_service"
	pb "proto/guild"
	matchpb "proto/match"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func lobbyActor(t *testing.T, ctx context.Context, player uint64, account string, revision uint64) context.Context {
	t.Helper()
	out, e := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, e)
	return out
}
func consentedSQLLobby(t *testing.T, ctx context.Context, repo *GuildRepo, id, guild uint64, activity uint32, initiator, invitee uint64) *TrialLobbyRepo {
	t.Helper()
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return id, nil })
	require.NoError(t, e)
	result, e := store.Create(ctx, TrialLobbyCreate{GuildID: guild, ActivityID: activity, Roster: []uint64{initiator, invitee}, TTL: 10 * time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, result.Status)
	role, ok := roleidentity.Original(ctx)
	require.True(t, ok)
	accepted, e := store.Respond(lobbyActor(t, ctx, invitee, role.Account, role.OwnerRevision), id, invitee, true, 1, 5*time.Second)
	require.NoError(t, e)
	require.Equal(t, TrialLobbyRespondLaunch, accepted.Status)
	return store
}
func seedLobbyOwners(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, map[uint64]uint32{22: 1})
	for _, p := range []uint64{11, 22} {
		fencetest.Seed(t, ctx, db, p, "seller", 1)
	}
}

func TestTiDBTrialSQLLobbyRedisLossPendingAndOriginalOwner(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 22, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 301, nil })
	require.NoError(t, e)
	created, e := store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{22, 11}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, created.Status)
	require.NoError(t, repo.rdb.FlushDB(ctx).Err())
	copyStore, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 302, nil })
	require.NoError(t, e)
	loaded, e := copyStore.LoadForPlayer(actor, 11, ^uint64(0))
	require.NoError(t, e)
	require.Equal(t, created.Lobby, loaded, "显示时间与Redis丢失不能撤销SQL邀请")
	var raw []byte
	require.NoError(t, db.QueryRowContext(ctx, "SELECT original_proto FROM guild_trial_lobby WHERE lobby_id=301").Scan(&raw))
	original := &pb.GuildTrialLobbyOriginal{}
	require.NoError(t, proto.Unmarshal(raw, original))
	require.EqualValues(t, 22, original.InitiatorPlayerId)
	require.EqualValues(t, 11, original.Roster[0].PlayerId)
	public, e := json.Marshal(loaded)
	require.NoError(t, e)
	require.NotContains(t, string(public), "seller")
	fencetest.Update(t, ctx, db, 11, "buyer", 2, 0)
	_, e = copyStore.Respond(lobbyActor(t, ctx, 11, "buyer", 2), 301, 11, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	fencetest.Update(t, ctx, db, 11, "seller", 3, 0)
	_, e = copyStore.Respond(lobbyActor(t, ctx, 11, "seller", 3), 301, 11, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	var state uint32
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM guild_trial_lobby WHERE lobby_id=301").Scan(&state))
	require.EqualValues(t, 1, state)
}

func TestTiDBTrialSQLLastConsentAndUnknownLaunchNeverTTLRelease(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store := consentedSQLLobby(t, actor, repo, 311, 44, 3, 11, 22)
	for i := 0; i < 2; i++ {
		reply, e := store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 311, 22, true, 1, time.Second)
		require.NoError(t, e)
		require.Equal(t, TrialLobbyRespondNotPending, reply.Status)
		require.True(t, reply.AlreadyAccepted)
	}
	lb, e := store.Load(ctx, 311)
	require.NoError(t, e)
	require.Equal(t, lb.State, lb.EffectiveAt(^uint64(0)).State)
	roster, e := store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 311
	prepared, e := activities.PrepareTrialLaunch(actor, 311, battle, 1, roster, testNowMs)
	require.NoError(t, e)
	_, e = db.ExecContext(ctx, "UPDATE guild_trial_lobby SET launch_deadline_ms=1 WHERE lobby_id=311")
	require.NoError(t, e)
	_, e = store.Finish(ctx, 311, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, 0, 0, nil)
	require.ErrorIs(t, e, ErrWriteConflict, "未知RPC不能当开战失败解除占位")
	_, e = store.ExpireUnpreparedSQL(ctx, 0)
	require.NoError(t, e)
	var occupied int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=311").Scan(&occupied))
	require.Equal(t, 2, occupied)
	require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, &matchpb.CancelActivityBattleResponse{RequestCancelled: true}, testNowMs+1))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=311").Scan(&occupied))
	require.Zero(t, occupied)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby WHERE lobby_id=311 AND state=4").Scan(&count))
	require.Equal(t, 1, count)
}

func TestTiDBTrialCancelBeforePrepareSurvivesCrashAndRejectsLateLaunch(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store := consentedSQLLobby(t, actor, repo, 351, 44, 3, 11, 22)
	lb, e := store.Load(ctx, 351)
	require.NoError(t, e)
	roster, e := store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	var before []byte
	require.NoError(t, db.QueryRowContext(ctx, "SELECT original_proto FROM guild_trial_lobby WHERE lobby_id=351").Scan(&before))
	// 末人同意已提交，进程在任何Prepare/Start前消失；固定截止已到，不依赖sleep或RedisTTL。
	_, e = db.ExecContext(ctx, "UPDATE guild_trial_lobby SET launch_deadline_ms=1 WHERE lobby_id=351")
	require.NoError(t, e)
	_, e = store.ExpireUnpreparedSQL(ctx, 0)
	require.NoError(t, e)
	finished, e := store.Load(ctx, 351)
	require.NoError(t, e)
	require.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, finished.State)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 351
	_, e = activities.PrepareTrialLaunch(actor, 351, battle, 1, roster, testNowMs)
	require.ErrorIs(t, e, ErrWriteConflict)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch WHERE request_id=351").Scan(&count))
	require.Zero(t, count, "没有许可原件，调用者不能发送Start RPC")
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=351").Scan(&count))
	require.Zero(t, count)
	var after []byte
	require.NoError(t, db.QueryRowContext(ctx, "SELECT original_proto FROM guild_trial_lobby WHERE lobby_id=351").Scan(&after))
	require.Equal(t, before, after, "取消只推进状态，邀请原件不变")
}

func TestTiDBTrialExpiredUnpreparedCleanupDoesNotRegrantChangedOwner(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 355, nil })
	require.NoError(t, e)
	_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	row, e := scanTrialLobby(db.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=355"))
	require.NoError(t, e)
	_, o, _, e := lobbyView(row)
	require.NoError(t, e)
	// 隔离库固定过去的邀请原件，无墙钟sleep；保留邀请捕获的seller/rev1，不拿新owner重写旧consent。
	o.CreatedMs = 1
	o.ExpiresMs = 2
	v := currentLobbyView(row)
	v.ExpireAtMs = 2
	raw, e := proto.MarshalOptions{Deterministic: true}.Marshal(o)
	require.NoError(t, e)
	view, e := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	require.NoError(t, e)
	_, e = db.ExecContext(ctx, "UPDATE guild_trial_lobby SET created_ms=1,expires_ms=2,original_proto=?,view_proto=? WHERE lobby_id=355", raw, view)
	require.NoError(t, e)
	fencetest.Update(t, ctx, db, 22, "buyer", 2, 0)
	_, e = store.ExpirePendingSQL(ctx, 0)
	require.NoError(t, e, "取消未许可外部动作的旧邀请，不重新授予旧owner权限")
	lb, e := store.Load(ctx, 355)
	require.NoError(t, e)
	require.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, lb.State)
	_, e = store.Respond(lobbyActor(t, ctx, 22, "buyer", 2), 355, 22, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 355
	_, e = activities.PrepareTrialLaunch(actor, 355, battle, 1, o.Roster, testNowMs)
	require.ErrorIs(t, e, ErrWriteConflict)
	var occupied int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=355").Scan(&occupied))
	require.Zero(t, occupied)
	fence, e := rolefence.Read(ctx, db, 22)
	require.NoError(t, e)
	require.Equal(t, "buyer", fence.Account)
	require.EqualValues(t, 2, fence.OwnerRevision)
}

// 同意之后Match先写了在途票或整队开战(占用租约丢了之后才可能),Trial不能提交ACTIVE后才依靠远端拒绝。
// 票和整队开战的判据本身在 go/match/internal/authority/occupancy_native_test.go。
func TestTiDBTrialPrepareReverseGuardAfterConsents(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store := consentedSQLLobby(t, actor, repo, 361, 44, 3, 11, 22)
	lb, e := store.Load(ctx, 361)
	require.NoError(t, e)
	roster, e := store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	occupancy := occupancyOf(repo)
	occupancy.setAcquireErr(ErrWriteConflict)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 361
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.ErrorIs(t, e, ErrWriteConflict)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE request_id=361").Scan(&count))
	require.Zero(t, count)
	require.Empty(t, occupancy.released(), "大厅还在,占用不放")
	occupancy.setAcquireErr(ErrTrialMatchUnavailable)
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.ErrorIs(t, e, ErrTrialMatchUnavailable, "问不到 match 不是业务拒绝")
	occupancy.setAcquireErr(nil)
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.NoError(t, e)
	last := occupancy.lastAcquire()
	require.EqualValues(t, 361, last.owner)
	require.Equal(t, trialOccupancyMargin, last.ttl)
	requireSameRoster(t, roster, last.roster, "Prepare 带大厅原件的全部身份")
	// 已有原件的重放不再占:Start 可能已经写了票,这时 match 一定拒绝。
	calls := occupancy.acquireCount()
	occupancy.setAcquireErr(ErrWriteConflict)
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.NoError(t, e)
	require.Equal(t, calls, occupancy.acquireCount())
}

func TestTrialLobbyDecoderKeepsInitiatorSeparateFromSortedAuthorization(t *testing.T) {
	o := &pb.GuildTrialLobbyOriginal{LobbyId: 1, GuildId: 44, ActivityId: 3, InitiatorPlayerId: 22, CreatedMs: 1, ExpiresMs: 10, Roster: []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22, Account: "seller", OwnerRevision: 1}}}
	v := &pb.GuildTrialLobbyView{LobbyId: 1, InitiatorPlayerId: 22, MemberPlayerIds: []uint64{22, 11}, AcceptedPlayerIds: []uint64{22}, State: pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, ExpireAtMs: 10}
	c := &pb.GuildTrialLobbyConsents{Consents: []*pb.GuildTrialLobbyConsent{{LobbyId: 1, Identity: o.Roster[1]}}}
	encode := func(message proto.Message) []byte {
		raw, e := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		require.NoError(t, e)
		return raw
	}
	row := &pb.GuildTrialLobbyRecord{LobbyId: 1, GuildId: 44, ActivityId: 3, InitiatorPlayerId: 22, State: 1, CreatedMs: 1, ExpiresMs: 10, OriginalProto: encode(o), ViewProto: encode(v), ConsentProto: encode(c)}
	lb, original, _, e := lobbyView(row)
	require.NoError(t, e)
	require.EqualValues(t, 22, lb.InitiatorID)
	require.EqualValues(t, 11, original.Roster[0].PlayerId)
	// 显示序列交换不会给最小player发起权限，也不改变原授权集合。
	v.MemberPlayerIds = []uint64{11, 22}
	row.ViewProto = encode(v)
	lb, _, _, e = lobbyView(row)
	require.NoError(t, e)
	require.EqualValues(t, 22, lb.InitiatorID)
	c.Consents[0].Identity = &pb.GuildTrialRoleIdentity{PlayerId: 22, Account: "buyer", OwnerRevision: 2}
	row.ConsentProto = encode(c)
	_, _, _, e = lobbyView(row)
	require.ErrorIs(t, e, ErrWriteConflict)
}

func TestTiDBTrialDisbandRetainsUntilBoundTerminalAndOldResultCannotClearNewLobby(t *testing.T) {
	for _, poison := range []bool{false, true} {
		name := "settle"
		if poison {
			name = "poison"
		}
		t.Run(name, func(t *testing.T) {
			ctx, db, repo, activities := guildRoleTiDB(t)
			seedLobbyOwners(t, ctx, db)
			actor := lobbyActor(t, ctx, 11, "seller", 1)
			store := consentedSQLLobby(t, actor, repo, 371, 44, 3, 11, 22)
			lb, e := store.Load(ctx, 371)
			require.NoError(t, e)
			roster, e := store.LoadConsents(ctx, lb)
			require.NoError(t, e)
			battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
			battle.LaunchRequestID = 371
			_, e = activities.PrepareTrialLaunch(actor, 371, battle, 1, roster, testNowMs)
			require.NoError(t, e)
			require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
			_, e = repo.DisbandGuild(actor, 44, 11, testNowMs+1)
			require.NoError(t, e)
			var occupied int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=371").Scan(&occupied))
			require.Equal(t, 2, occupied, "解散不是取消证明")
			require.NotContains(t, occupancyOf(repo).released(), uint64(371), "解散不放占用")
			var state uint32
			require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM guild_trial_launch WHERE request_id=371").Scan(&state))
			require.EqualValues(t, 2, state)
			bad := battle
			bad.PeriodKey++
			if poison {
				_, e = activities.MarkTrialBattlePoison(ctx, bad, testNowMs, testNowMs+2)
			} else {
				_, e = activities.SettleTrialBattleTx(ctx, TrialSettleInput{Battle: bad, Activity: actTrialRow(), FinishedAtMs: testNowMs, NowMs: testNowMs + 2})
			}
			require.ErrorIs(t, e, ErrWriteConflict, "缺帮会也必须核真实原上下文")
			ev := &kafkapb.BattleResultEvent{BattleId: 900, MatchMode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), BattleConfigId: 1, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, FinishedAtMs: testNowMs, Teams: []*kafkapb.BattleResultTeam{{TeamIndex: 0, PlayerIds: []uint64{22, 11}}}, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: 44, ActivityId: battle.ActivityID, PeriodKey: battle.PeriodKey, GuildPeriodKey: battle.GuildPeriodKey, InitiatorPlayerId: 11, LaunchRequestId: 371}}
			require.NoError(t, activities.ValidateTrialTerminal(ctx, ev))
			for _, mutate := range []func(*kafkapb.BattleResultEvent){func(v *kafkapb.BattleResultEvent) { v.Teams[0].PlayerIds = []uint64{11} }, func(v *kafkapb.BattleResultEvent) { v.BattleConfigId = 2 }, func(v *kafkapb.BattleResultEvent) { v.FinishedAtMs = 0 }, func(v *kafkapb.BattleResultEvent) { v.Outcome = battlepb.EBattleOutcome_BATTLE_OUTCOME_ONGOING }} {
				invalid := proto.Clone(ev).(*kafkapb.BattleResultEvent)
				mutate(invalid)
				require.ErrorIs(t, activities.ValidateTrialTerminal(ctx, invalid), ErrWriteConflict)
			}
			if poison {
				out, e := activities.MarkTrialBattlePoison(ctx, battle, testNowMs, testNowMs+2)
				require.NoError(t, e)
				require.Equal(t, TrialMarkGuildGone, out)
			} else {
				out, e := activities.SettleTrialBattleTx(ctx, TrialSettleInput{Battle: battle, Activity: actTrialRow(), FinishedAtMs: testNowMs, NowMs: testNowMs + 2})
				require.NoError(t, e)
				require.Equal(t, TrialSettleGuildGone, out.Status)
			}
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=371").Scan(&occupied))
			require.Zero(t, occupied)
			require.Contains(t, occupancyOf(repo).released(), uint64(371), "原 launch 终结,占用随之放开")
			for _, table := range []string{"guild", "guild_trial_battle", "guild_asset_op", "guild_trial_reward_owed"} {
				var count int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
				require.Zero(t, count, "真实终局仅关闭原合同，不重建帮会或发奖")
			}
			// 玩家此后在新帮会接受新邀请；旧结果只能命中旧永久墓碑。
			seedManagedGuild(t, ctx, db, 45, 1, 1, 30, 11, map[uint64]uint32{22: 1})
			consentedSQLLobby(t, actor, repo, 372, 45, 3, 11, 22)
			if poison {
				_, e = activities.MarkTrialBattlePoison(ctx, battle, testNowMs, testNowMs+3)
			} else {
				_, e = activities.SettleTrialBattleTx(ctx, TrialSettleInput{Battle: battle, Activity: actTrialRow(), FinishedAtMs: testNowMs, NowMs: testNowMs + 3})
			}
			require.NoError(t, e)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot WHERE lobby_id=372").Scan(&occupied))
			require.Equal(t, 2, occupied, "旧终局不能清新lobby占位")
		})
	}
}

// match 拒绝占用(票先赢)时不建房;同意时被拒不留半份同意。判据本身在 go/match/internal/authority/occupancy_native_test.go。
func TestTiDBTrialMatchWinsBeforePrivateCreateAndNoPartialConsent(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 321, nil })
	require.NoError(t, e)
	occupancy := occupancyOf(repo)
	occupancy.setAcquireErr(ErrWriteConflict)
	_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.ErrorIs(t, e, ErrWriteConflict)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby").Scan(&count))
	require.Zero(t, count)
	require.Equal(t, []uint64{321}, occupancy.released(), "放开挂在 Acquire 之前,确定拒绝时是空操作")
	occupancy.setAcquireErr(nil)
	created, e := store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, created.Status)
	// 建房时发起人带会话身份,对端留空由 match 捕获;租约盖住邀请有效期。
	first := occupancy.lastAcquire()
	require.EqualValues(t, 321, first.owner)
	require.Equal(t, time.Minute+trialOccupancyMargin, first.ttl)
	requireSameRoster(t, []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22}}, first.roster, "建房")
	occupancy.setAcquireErr(ErrWriteConflict)
	_, e = store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 321, 22, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	lb, e := store.Load(ctx, 321)
	require.NoError(t, e)
	require.Equal(t, []uint64{11}, lb.Accepted())
	occupancy.setAcquireErr(nil)
	calls := occupancy.acquireCount()
	reply, e := store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 321, 22, true, 1, time.Second)
	require.NoError(t, e)
	require.Equal(t, TrialLobbyRespondLaunch, reply.Status)
	require.Equal(t, calls+1, occupancy.acquireCount(), "同意之前续租")
	renewed := occupancy.lastAcquire()
	require.Equal(t, time.Minute+trialOccupancyMargin, renewed.ttl, "续租按邀请时长")
	requireSameRoster(t, []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22, Account: "seller", OwnerRevision: 1}}, renewed.roster, "续租带大厅原件的全部身份")
	// 重复同意的结论在事务里出,不再问 match。
	_, e = store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 321, 22, true, 1, time.Second)
	require.NoError(t, e)
	require.Equal(t, calls+1, occupancy.acquireCount())
}

// 规格 §4:事务提交失败就放开,大厅结束也放开。本库先答的拒绝不走到 match。
func TestTiDBTrialOccupancyIsReleasedWhenLobbyIsNotCreatedOrEnds(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	occupancy := occupancyOf(repo)
	newStore := func(id uint64) *TrialLobbyRepo {
		store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return id, nil })
		require.NoError(t, e)
		return store
	}
	// 事务里才发现的拒绝(帮会不存在):占用已拿,要放开。
	_, e := newStore(401).Create(actor, TrialLobbyCreate{GuildID: 999, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.Error(t, e)
	require.Equal(t, []uint64{401}, occupancy.released())

	// 建房成功不放;发起人取消就放。
	created, e := newStore(402).Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, Cooldown: time.Hour, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, created.Status)
	require.Equal(t, []uint64{401}, occupancy.released())

	// 别的大厅占着、发起人冷却中:本库先答,不问 match。
	calls := occupancy.acquireCount()
	busy, e := newStore(403).Create(lobbyActor(t, ctx, 22, "seller", 1), TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{22, 11}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyMemberBusy, busy.Status)
	require.Equal(t, calls, occupancy.acquireCount())

	reply, e := newStore(402).Respond(actor, 402, 11, false, 1, time.Second)
	require.NoError(t, e)
	require.Equal(t, TrialLobbyRespondDeclined, reply.Status)
	require.Equal(t, []uint64{401, 402}, occupancy.released())

	cooling, e := newStore(404).Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCooldown, cooling.Status)
	require.Equal(t, calls, occupancy.acquireCount())

	// 取消证明之后放开。launch 还在途时大厅结束不了,占用也不放。
	_, e = db.ExecContext(ctx, "UPDATE guild_trial_lobby_slot SET cooldown_until_ms=0")
	require.NoError(t, e)
	store := consentedSQLLobby(t, actor, repo, 405, 44, 3, 11, 22)
	lb, e := store.Load(ctx, 405)
	require.NoError(t, e)
	roster, e := store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 405
	prepared, e := activities.PrepareTrialLaunch(actor, 405, battle, 1, roster, testNowMs)
	require.NoError(t, e)
	_, e = store.Finish(ctx, 405, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, 0, 0, nil)
	require.ErrorIs(t, e, ErrWriteConflict)
	require.Equal(t, []uint64{401, 402}, occupancy.released())
	require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, &matchpb.CancelActivityBattleResponse{RequestCancelled: true}, testNowMs+1))
	require.Equal(t, []uint64{401, 402, 405}, occupancy.released())

	// 结算之后放开:票终局到结算之间,这一行接着挡新入场。
	store = consentedSQLLobby(t, actor, repo, 406, 44, 3, 11, 22)
	lb, e = store.Load(ctx, 406)
	require.NoError(t, e)
	roster, e = store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	battle = actTrialKey(actTrialRow(), 901, 44, 11, testNowMs)
	battle.LaunchRequestID = 406
	_, e = activities.PrepareTrialLaunch(actor, 406, battle, 1, roster, testNowMs)
	require.NoError(t, e)
	require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
	require.Equal(t, []uint64{401, 402, 405}, occupancy.released())
	_, e = activities.SettleTrialBattleTx(ctx, TrialSettleInput{Battle: battle, Activity: actTrialRow(), Candidates: []uint64{11, 22}, NowMs: testNowMs + 2})
	require.NoError(t, e)
	require.Equal(t, []uint64{401, 402, 405, 406}, occupancy.released())
}

// 规格 §4:Acquire 超时时 match 可能已经写了占用,没建成房也要放,不能让这批人等到租约到期。
func TestTiDBTrialOccupancyIsReleasedWhenAcquireOutcomeIsUnknown(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 411, nil })
	require.NoError(t, e)
	occupancy := occupancyOf(repo)
	occupancy.setAcquireErr(ErrTrialMatchUnavailable)
	_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.ErrorIs(t, e, ErrTrialMatchUnavailable)
	require.Equal(t, []uint64{411}, occupancy.released())
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby").Scan(&count))
	require.Zero(t, count)
}

// 规格 §4:同意之前的续租排在别人结束大厅之后,把占用又写回去了。同意的事务看到大厅已结束,补一次放开,不留到租约到期。
func TestTiDBTrialOccupancyIsReleasedWhenRenewalLosesToLobbyEnd(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	initiator := lobbyActor(t, ctx, 11, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 412, nil })
	require.NoError(t, e)
	occupancy := occupancyOf(repo)
	created, e := store.Create(initiator, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, created.Status)
	require.Empty(t, occupancy.released())

	occupancy.beforeAcquire(func() {
		declined, e := store.Respond(initiator, 412, 11, false, 1, time.Second)
		require.NoError(t, e)
		require.Equal(t, TrialLobbyRespondDeclined, declined.Status)
		require.Equal(t, []uint64{412}, occupancy.released())
	})
	reply, e := store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 412, 22, true, 1, time.Second)
	require.NoError(t, e)
	require.Equal(t, TrialLobbyRespondNotPending, reply.Status)
	require.Equal(t, []uint64{412, 412}, occupancy.released(), "续租在第一次放开之后,要补一次")
}

func TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	results := make(chan TrialLobbyCreateResult, 2)
	for i, ids := range [][]uint64{{11, 22}, {22, 11}} {
		id := uint64(331 + i)
		ids := ids
		actor := lobbyActor(t, ctx, ids[0], "seller", 1)
		store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return id, nil })
		require.NoError(t, e)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: ids, TTL: time.Minute, NowMs: 1})
			errs <- e
			results <- out
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for e := range errs {
		require.NoError(t, e)
	}
	created, busy := 0, 0
	for out := range results {
		if out.Status == TrialLobbyCreated {
			created++
		}
		if out.Status == TrialLobbyMemberBusy {
			busy++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, busy)
}

func TestTiDBTrialRoleFencePrefixRejectsBeforeLobbyOrConsentWrite(t *testing.T) {
	for _, condition := range []string{"frozen_role", "missing_role"} {
		t.Run(condition, func(t *testing.T) {
			ctx, db, repo, _ := guildRoleTiDB(t)
			seedLobbyOwners(t, ctx, db)
			actor := lobbyActor(t, ctx, 11, "seller", 1)
			store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 381, nil })
			require.NoError(t, e)
			switch condition {
			case "frozen_role":
				fencetest.Update(t, ctx, db, 22, "seller", 1, 901)
			case "missing_role":
				_, e = db.ExecContext(ctx, "DELETE FROM role_fence WHERE player_id=22")
				require.NoError(t, e)
			}
			_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
			require.ErrorIs(t, e, ErrWriteConflict)
			require.Equal(t, []uint64{381}, occupancyOf(repo).released(), "占用已拿,房间没建成要放开")
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby").Scan(&count))
			require.Zero(t, count)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot").Scan(&count))
			require.Zero(t, count)
		})
	}
}
