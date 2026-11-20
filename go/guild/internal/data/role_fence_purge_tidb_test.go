package data

import (
	"context"
	"database/sql"
	"testing"
	"time"

	pb "proto/guild"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

const guildPurgeSecret = "fixture-only-coordinator-secret-0000"

// guildPurge 走真的 PurgeRemovedRole:鉴权、按版本合并墓碑、锁副本、清理,同一个事务。
func guildPurge(t *testing.T, ctx context.Context, participant *rolefence.ParticipantServer, state *rfpb.RoleFenceState) *rfpb.PurgeRemovedRoleResponse {
	t.Helper()
	in := &rfpb.PurgeRemovedRoleRequest{State: state}
	require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, guildPurgeSecret, uint64(time.Now().UnixMilli())))
	out, err := participant.PurgeRemovedRole(ctx, in)
	require.NoError(t, err)
	return out
}

// guildPurgeFunc 是数据层用例装进参与者的清理:只跑数据库部分,把每次的结论记进 last。
// 生产装的是 logic 的 RoleFencePurge,提交之后的缓存失效、推送由它登记(见 logic 的 TestAfterRolePurge*)。
func guildPurgeFunc(last *RolePurgeResult) rolefence.PurgeFunc {
	return func(ctx context.Context, tx *sql.Tx, player uint64) error {
		res, err := PurgeRemovedRole(ctx, tx, player, testNowMs)
		*last = res
		return err
	}
}

func guildLeaderOf(t *testing.T, ctx context.Context, db *sql.DB, guildID uint64) uint64 {
	t.Helper()
	var leader uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT leader_id FROM guild WHERE guild_id=?", guildID).Scan(&leader))
	return leader
}

func guildPlayerRows(t *testing.T, ctx context.Context, db *sql.DB, table string, player uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"` WHERE player_id=?", player).Scan(&n))
	return n
}

// 删角收尾(docs/design/account-deletion.md §6.2):他的成员行、状态行、申请、seq、每日计数、幂等键都删掉,
// 终态资产指令与历练占位留着,同帮其他人一行不动。第二次调用没有可删的行,照样成功。
func TestTiDBRoleFencePurgeDeletesPlayerRowsAndKeepsHistory(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	const removed, officer = uint64(22), uint64(23)
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, map[uint64]uint32{removed: constants.RoleMember, officer: constants.RoleOfficer})
	for _, player := range []uint64{removed, officer} {
		mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (?, 1)", player)
		seedApplicationRow(t, ctx, db, 45, player, 1, testNowMs+3600000)
		insertPendingFixture(t, ctx, db, guildPlayerOpSeqTable, &pb.GuildPlayerOpSeqRecord{PlayerId: player, Stream: 1, NextSeq: 2, UpdatedMs: 1, Epoch: 1})
		insertPendingFixture(t, ctx, db, "guild_daily_counter", &pb.GuildDailyCounterRecord{PlayerId: player, CounterKind: pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, RefId: 7, PeriodKey: 20261119, UsedCount: 1, UpdatedMs: 1})
		insertPendingFixture(t, ctx, db, guildClientRequestTable, &pb.GuildClientRequestRecord{PlayerId: player, ClientRequestId: 9, OpId: 300 + player, CreatedMs: 1})
	}
	// 同一玩家多行:逐行点删要把每一行都删掉。
	insertPendingFixture(t, ctx, db, guildPlayerOpSeqTable, &pb.GuildPlayerOpSeqRecord{PlayerId: removed, Stream: 2, NextSeq: 1, UpdatedMs: 1, Epoch: 1})
	insertPendingFixture(t, ctx, db, "guild_daily_counter", &pb.GuildDailyCounterRecord{PlayerId: removed, CounterKind: pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, RefId: 8, PeriodKey: 202647, UsedCount: 3, UpdatedMs: 1})
	seedApplicationRow(t, ctx, db, 46, removed, 1, testNowMs+3600000)
	insertPendingFixture(t, ctx, db, "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: removed, GuildId: 44, Seq: 1, Status: pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED})
	insertPendingFixture(t, ctx, db, "guild_trial_lobby_slot", &pb.GuildTrialLobbySlotRecord{PlayerId: removed})
	fencetest.Seed(t, ctx, db, removed, "seller", 1)

	var last RolePurgeResult
	participant, err := rolefence.NewParticipantServer(db, RoleFencePending, guildPurgeFunc(&last), guildPurgeSecret)
	require.NoError(t, err)
	tombstone := &rfpb.RoleFenceState{PlayerId: removed, OwnerRevision: 2, Version: 2, HomeZone: 1}
	for round := 0; round < 2; round++ {
		require.False(t, guildPurge(t, ctx, participant, tombstone).GetRetry())
		if round == 0 {
			require.Equal(t, RolePurgeResult{Player: removed, GuildID: 44, ZoneID: 1, Members: []uint64{11, officer}}, last,
				"普通成员按退帮处理,推送给留下的人")
		} else {
			require.Equal(t, RolePurgeResult{Player: removed}, last, "第二次已不在帮,提交之后无事可做")
		}
		for _, table := range []string{"guild_member", "guild_player_state", "guild_application", guildPlayerOpSeqTable, "guild_daily_counter", guildClientRequestTable} {
			require.Zero(t, guildPlayerRows(t, ctx, db, table, removed), table)
			require.Equal(t, 1, guildPlayerRows(t, ctx, db, table, officer), "别人的 %s 不动", table)
		}
		require.Equal(t, 1, guildPlayerRows(t, ctx, db, "guild_asset_op", removed), "终态资产指令是审计,保留")
		require.Equal(t, 1, guildPlayerRows(t, ctx, db, "guild_trial_lobby_slot", removed), "历练记录保留")
		var members int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member WHERE guild_id=44").Scan(&members))
		require.Equal(t, 2, members, "帮主和长老还在")
		fence, err := rolefence.Read(ctx, db, removed)
		require.NoError(t, err)
		require.Empty(t, fence.Account, "副本已合并成墓碑")
	}
}

// 帮主被删、帮里还有人(§6.3):帮主交给职位最高的人,同职位比累计帮贡,再比入帮早晚。
// 23、24 同是长老,24 帮贡多,入帮更晚也是他;22 帮贡最多但只是成员。他的成员行、状态行删掉,帮里恰好一个帮主。
func TestTiDBRoleFencePurgeHandsLeadershipToTopOfficer(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, map[uint64]uint32{
		22: constants.RoleMember, 23: constants.RoleOfficer, 24: constants.RoleOfficer})
	for _, m := range []struct{ player, contribution, joinedMs uint64 }{
		{22, 900, testNowMs - 3000}, {23, 100, testNowMs - 2000}, {24, 300, testNowMs - 1000},
	} {
		mustExec(t, ctx, db, "UPDATE guild_member SET contribution_total=?, join_time_ms=? WHERE guild_id=44 AND player_id=?",
			m.contribution, m.joinedMs, m.player)
	}
	mustExec(t, ctx, db, "INSERT INTO guild_player_state (player_id, updated_ms) VALUES (11, 1)")
	fencetest.Seed(t, ctx, db, 11, "seller", 1)
	var last RolePurgeResult
	participant, err := rolefence.NewParticipantServer(db, RoleFencePending, guildPurgeFunc(&last), guildPurgeSecret)
	require.NoError(t, err)
	tombstone := &rfpb.RoleFenceState{PlayerId: 11, OwnerRevision: 2, Version: 2, HomeZone: 1}

	require.False(t, guildPurge(t, ctx, participant, tombstone).GetRetry())
	require.Equal(t, RolePurgeResult{Player: 11, GuildID: 44, ZoneID: 1, Successor: 24, Members: []uint64{22, 23, 24}}, last)
	require.Equal(t, uint64(24), guildLeaderOf(t, ctx, db, 44))
	for player, role := range map[uint64]uint32{22: constants.RoleMember, 23: constants.RoleOfficer, 24: constants.RoleLeader} {
		var got uint32
		require.NoError(t, db.QueryRowContext(ctx, "SELECT role FROM guild_member WHERE guild_id=44 AND player_id=?", player).Scan(&got))
		require.Equal(t, role, got, "player %d", player)
	}
	var leaders int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member WHERE guild_id=44 AND role=?", constants.RoleLeader).Scan(&leaders))
	require.Equal(t, 1, leaders, "恰好一个帮主")
	require.Zero(t, guildPlayerRows(t, ctx, db, "guild_member", 11))
	require.Zero(t, guildPlayerRows(t, ctx, db, "guild_player_state", 11))

	require.False(t, guildPurge(t, ctx, participant, tombstone).GetRetry())
	require.Equal(t, RolePurgeResult{Player: 11}, last, "第二次已不在帮,不再让位")
	require.Equal(t, uint64(24), guildLeaderOf(t, ctx, db, 44))
}

// 帮主是帮里最后一个人(§6.3):按 DisbandGuild 的同一段整帮解散,帮会的待审申请一起删。归属区取 guild 行(摘榜用),
// 不是角色的 HomeZone。老成员可能从没建过状态行,收尾在事务里补建再删,不留行。
func TestTiDBRoleFencePurgeDisbandsSoloGuild(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	seedManagedGuild(t, ctx, db, 44, 2, 1, 30, 11, nil)
	seedApplicationRow(t, ctx, db, 44, 33, 1, testNowMs+3600000)
	fencetest.Seed(t, ctx, db, 11, "seller", 1)
	var last RolePurgeResult
	participant, err := rolefence.NewParticipantServer(db, RoleFencePending, guildPurgeFunc(&last), guildPurgeSecret)
	require.NoError(t, err)
	tombstone := &rfpb.RoleFenceState{PlayerId: 11, OwnerRevision: 2, Version: 2, HomeZone: 1}

	require.False(t, guildPurge(t, ctx, participant, tombstone).GetRetry())
	require.Equal(t, RolePurgeResult{Player: 11, GuildID: 44, ZoneID: 2, Disbanded: true}, last)
	var guilds int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild WHERE guild_id=44").Scan(&guilds))
	require.Zero(t, guilds, "帮会已解散")
	require.Zero(t, guildPlayerRows(t, ctx, db, "guild_member", 11))
	require.Zero(t, guildPlayerRows(t, ctx, db, "guild_player_state", 11), "补建的状态行随个人数据一起删")
	require.Zero(t, guildApplicationCount(t, ctx, db, 44, 33), "解散删本帮的待审申请")

	require.False(t, guildPurge(t, ctx, participant, tombstone).GetRetry())
	require.Equal(t, RolePurgeResult{Player: 11}, last, "第二次帮会已不在,无事可做")
}

// 删角只在试探时挡帮主(guild_leader:申请注销时的预检,让玩家自己先转让);真正冻结放行,收尾自动让位或解散
// (TestTiDBRoleFencePurgeHandsLeadershipToTopOfficer)。过户照旧是在帮就挡(guild_member),试探与否一样。
// 普通成员删角不挡见 TestTiDBPendingGuildMembershipBlocksFreezeOnly。
func TestTiDBPendingGuildLeaderBlocksRemovalProbeOnly(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, nil)
	probing := rolefence.WithProbing(ctx)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		token   uint64
		blocker string
	}{
		{"删角试探", probing, pendingRemoval, "guild_leader"},
		{"删角冻结", ctx, pendingRemoval, ""},
		{"过户试探", probing, pendingListing, "guild_member"},
		{"过户冻结", ctx, pendingListing, "guild_member"},
	} {
		blocker, err := runPending(t, tc.ctx, db, tc.token)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.blocker, blocker, tc.name)
	}
}
