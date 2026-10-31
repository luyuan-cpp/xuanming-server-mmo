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
		_, e := db.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,trade_listing_id) VALUES(?,'seller',?,1,1,0)", p, []byte{})
		require.NoError(t, e)
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
	_, e = db.ExecContext(ctx, "UPDATE account_role SET account='buyer',owner_revision=2 WHERE player_id=11")
	require.NoError(t, e)
	_, e = copyStore.Respond(lobbyActor(t, ctx, 11, "buyer", 2), 301, 11, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	_, e = db.ExecContext(ctx, "UPDATE account_role SET account='seller',owner_revision=3 WHERE player_id=11")
	require.NoError(t, e)
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
	_, e = db.ExecContext(ctx, "UPDATE account_role SET account='buyer',owner_revision=2 WHERE player_id=22")
	require.NoError(t, e)
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
	var account string
	var revision uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT account,owner_revision FROM account_role WHERE player_id=22").Scan(&account, &revision))
	require.Equal(t, "buyer", account)
	require.EqualValues(t, 2, revision)
}

func TestTiDBTrialPrepareReverseGuardAfterConsentsAndTeamUnknown(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store := consentedSQLLobby(t, actor, repo, 361, 44, 3, 11, 22)
	lb, e := store.Load(ctx, 361)
	require.NoError(t, e)
	roster, e := store.LoadConsents(ctx, lb)
	require.NoError(t, e)
	// 同意之后Match先取得同P并写在途票，Trial不能提交ACTIVE后才依靠远端拒绝。
	_, e = db.ExecContext(ctx, "INSERT INTO match_authority_ticket(player_id,state) VALUES(22,1)")
	require.NoError(t, e)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 361
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.ErrorIs(t, e, ErrWriteConflict)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE request_id=361").Scan(&count))
	require.Zero(t, count)
	_, e = db.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=22")
	require.NoError(t, e)
	_, e = db.ExecContext(ctx, "INSERT INTO team_launch(actor_id,request_high,request_low,state) VALUES(11,1,1,4)")
	require.NoError(t, e)
	_, e = db.ExecContext(ctx, "INSERT INTO team_launch_role(actor_id,request_high,request_low,player_id,state) VALUES(11,1,1,22,5)")
	require.NoError(t, e)
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.ErrorIs(t, e, ErrWriteConflict, "Team父4未有关闭正证明，role5也不放行")
	_, e = db.ExecContext(ctx, "UPDATE team_launch SET state=5 WHERE actor_id=11 AND request_high=1 AND request_low=1")
	require.NoError(t, e)
	_, e = activities.PrepareTrialLaunch(actor, 361, battle, 1, roster, testNowMs)
	require.NoError(t, e)
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

func TestTiDBTrialMatchWinsBeforePrivateCreateAndNoPartialConsent(t *testing.T) {
	ctx, db, repo, _ := guildRoleTiDB(t)
	seedLobbyOwners(t, ctx, db)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 321, nil })
	require.NoError(t, e)
	// Match先持P提交非空pending0；不能因其state0或expires已过而当成空票。
	_, e = db.ExecContext(ctx, "INSERT INTO match_authority_ticket(player_id,state,ticket,request_id,formation_id,mode,config_id,team_size,enqueued_ms,not_before_ms,zone_id,rating_centi,team_id,network_profile,expires_ms,selected_region,preferred_region) VALUES(22,0,'','pending-exit',0,0,0,0,0,0,0,0,0,?,0,0,0)", []byte{})
	require.NoError(t, e)
	_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.ErrorIs(t, e, ErrWriteConflict)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby").Scan(&count))
	require.Zero(t, count)
	_, e = db.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=22")
	require.NoError(t, e)
	created, e := store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
	require.NoError(t, e)
	require.Equal(t, TrialLobbyCreated, created.Status)
	_, e = db.ExecContext(ctx, "UPDATE match_authority_ticket SET state=255 WHERE player_id=22")
	require.NoError(t, e)
	_, e = store.Respond(lobbyActor(t, ctx, 22, "seller", 1), 321, 22, true, 1, time.Second)
	require.ErrorIs(t, e, ErrWriteConflict)
	lb, e := store.Load(ctx, 321)
	require.NoError(t, e)
	require.Equal(t, []uint64{11}, lb.Accepted())
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

func TestTiDBTrialRoutePrefixRejectsBeforeLobbyOrConsentWrite(t *testing.T) {
	for _, condition := range []string{"frozen_route", "missing_route"} {
		t.Run(condition, func(t *testing.T) {
			ctx, db, repo, _ := guildRoleTiDB(t)
			seedLobbyOwners(t, ctx, db)
			actor := lobbyActor(t, ctx, 11, "seller", 1)
			store, e := NewSQLTrialLobbyRepo(repo.rdb, repo, func(context.Context) (uint64, error) { return 381, nil })
			require.NoError(t, e)
			switch condition {
			case "frozen_route":
				_, e = db.ExecContext(ctx, "UPDATE player_route SET storage_id=1,placement_version=1,frozen_run_id='guild-it-maintenance' WHERE player_id=22")
			case "missing_route":
				_, e = db.ExecContext(ctx, "DELETE FROM player_route WHERE player_id=22")
			}
			require.NoError(t, e)
			_, e = store.Create(actor, TrialLobbyCreate{GuildID: 44, ActivityID: 3, Roster: []uint64{11, 22}, TTL: time.Minute, NowMs: 1})
			require.Error(t, e)
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby").Scan(&count))
			require.Zero(t, count)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_lobby_slot").Scan(&count))
			require.Zero(t, count)
		})
	}
}
