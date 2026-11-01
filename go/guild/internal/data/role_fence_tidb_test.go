package data

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	pb "proto/guild"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 规格 §3.3:从目录自愈的路径。副本缺行时重试一次,真冻结第二次拒绝,新主自愈后通过,旧主不触发自愈直接拒绝。
func healRepo(t *testing.T) (context.Context, *sql.DB, *GuildRepo, *fencetest.Directory) {
	t.Helper()
	ctx, db, repo, _ := guildRoleTiDB(t)
	directory := fencetest.NewDirectory()
	repo.fence = rolefence.NewGuard(db, directory)
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, map[uint64]uint32{22: 1})
	return ctx, db, repo, directory
}

func TestTiDBRoleFenceMissingReplicaHealsAndRetriesOnce(t *testing.T) {
	ctx, db, repo, directory := healRepo(t)
	directory.Set(11, "seller", 1, 0)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	require.NoError(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "healed"), "副本缺行不是拒绝")
	require.Equal(t, []uint64{11}, directory.Reads, "只读一次目录")
	fence, err := rolefence.Read(ctx, db, 11)
	require.NoError(t, err)
	require.Equal(t, "seller", fence.Account)
	// 副本有了就不再问目录。
	require.NoError(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "no-rpc"))
	require.Len(t, directory.Reads, 1)
}

func TestTiDBRoleFenceFrozenRoleRejectsGuildWritesAndThawHeals(t *testing.T) {
	ctx, db, repo, directory := healRepo(t)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	_, err := rolefence.Apply(ctx, db, directory.Set(11, "seller", 1, 901))
	require.NoError(t, err)
	require.ErrorIs(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "frozen"), ErrWriteConflict)
	require.Len(t, directory.Reads, 1, "真冻结:自愈一次,第二次判定才拒绝")
	var text string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT announcement FROM guild WHERE guild_id=44").Scan(&text))
	require.Empty(t, text)
	require.ErrorIs(t, repo.CheckOriginalRole(actor), ErrWriteConflict, "预检读本库副本,同一个判据")
	// 解冻推送丢了:下一次访问自愈,照常放行。
	directory.Set(11, "seller", 1, 0)
	require.NoError(t, repo.CheckOriginalRole(actor))
	require.NoError(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "thawed"))
}

func TestTiDBRoleFenceHealLetsNewOwnerInAndKeepsOldOwnerOut(t *testing.T) {
	ctx, db, repo, directory := healRepo(t)
	oldOwner := lobbyActor(t, ctx, 11, "seller", 1)
	newOwner := lobbyActor(t, ctx, 11, "buyer", 2)
	// 过户之前全部参与者都已冻结;过户的推送丢了,副本停在冻结态。
	_, err := rolefence.Apply(ctx, db, directory.Set(11, "seller", 1, 901))
	require.NoError(t, err)
	directory.Set(11, "buyer", 2, 0)

	require.NoError(t, repo.CheckOriginalRole(newOwner), "新主修订号比副本大,自愈后通过")
	require.Len(t, directory.Reads, 1)
	require.NoError(t, repo.UpdateAnnouncementAuthorized(newOwner, 44, 11, "new-owner"))
	require.ErrorIs(t, repo.UpdateAnnouncementAuthorized(oldOwner, 44, 11, "old-owner"), ErrWriteConflict)
	require.ErrorIs(t, repo.CheckOriginalRole(oldOwner), ErrWriteConflict)
	require.Len(t, directory.Reads, 1, "旧主修订号比副本小,直接拒绝,不触发自愈")
}

func TestTiDBRoleFenceDirectoryOutageIsAConflictNotARejectionOfTheRole(t *testing.T) {
	ctx, _, repo, directory := healRepo(t)
	actor := lobbyActor(t, ctx, 11, "seller", 1)
	directory.Err = rolefence.ErrUnavailable
	require.ErrorIs(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "x"), ErrWriteConflict, "客户端稍后重试")
	directory.Err = nil
	// 目录里没有这名角色:和旧的 account_role 缺行一样不能写。
	require.ErrorIs(t, repo.UpdateAnnouncementAuthorized(actor, 44, 11, "x"), ErrWriteConflict)
}

// 没装配护栏不能算通过。
func TestCheckOriginalRoleRequiresFence(t *testing.T) {
	require.ErrorIs(t, (&GuildRepo{}).CheckOriginalRole(context.Background()), ErrWriteConflict)
	actor := lobbyActor(t, context.Background(), 11, "seller", 1)
	require.ErrorIs(t, (&GuildRepo{}).CheckOriginalRole(actor), ErrWriteConflict)
}

// 挂起全集:从目录原来的跨库守卫搬来的用例,改成直接测 RoleFencePending。

const pendingListing = uint64(88)

var pendingRemoval = rolefence.RemovalToken(11)

func runPending(t *testing.T, ctx context.Context, db *sql.DB, token uint64) (string, error) {
	t.Helper()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	return RoleFencePending(ctx, tx, &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: token, Version: 2, HomeZone: 1})
}

// requirePending 核过户和删角的结论都是 blocker。空串表示都放行。
func requirePending(t *testing.T, ctx context.Context, db *sql.DB, blocker string) {
	t.Helper()
	for name, token := range map[string]uint64{"过户": pendingListing, "删角": pendingRemoval} {
		got, err := runPending(t, ctx, db, token)
		require.NoError(t, err, name)
		require.Equal(t, blocker, got, name)
	}
}

// 夹具按正式 PB 列全集写入,不另建一份资产状态或表定义。
func insertPendingFixture(t *testing.T, ctx context.Context, db *sql.DB, table string, row proto.Message) {
	t.Helper()
	m := row.ProtoReflect()
	fields := m.Descriptor().Fields()
	columns, marks, values := []string{}, []string{}, []any{}
	for n := 0; n < fields.Len(); n++ {
		f := fields.Get(n)
		columns = append(columns, "`"+string(f.Name())+"`")
		marks = append(marks, "?")
		v := m.Get(f)
		switch f.Kind() {
		case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
			values = append(values, v.Uint())
		case protoreflect.Int32Kind, protoreflect.Int64Kind:
			values = append(values, v.Int())
		case protoreflect.BoolKind:
			values = append(values, v.Bool())
		case protoreflect.EnumKind:
			values = append(values, int32(v.Enum()))
		case protoreflect.StringKind:
			values = append(values, v.String())
		case protoreflect.BytesKind:
			values = append(values, append([]byte{}, v.Bytes()...))
		default:
			t.Fatalf("挂起夹具遇到未支持字段: %s", f.FullName())
		}
	}
	_, err := db.ExecContext(ctx, "INSERT INTO `"+table+"` ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(marks, ",")+")", values...)
	require.NoError(t, err)
}

func TestTiDBPendingGuildBlocksFreezeAndRemoval(t *testing.T) {
	for _, test := range []struct {
		name, table string
		row         proto.Message
	}{
		{"guild-pending", "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 1}},
		{"guild-manual", "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 5}},
		{"guild-unspecified", "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 0}},
		{"guild-unknown", "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 99}},
		{"guild-owed-after-leave", "guild_trial_reward_owed", &pb.GuildTrialRewardOwedRecord{PlayerId: 11, BattleId: 51}},
		{"guild-prepared-launch", "guild_trial_launch_role", &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 1}},
		{"guild-unknown-launch", "guild_trial_launch_role", &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 99}},
		{"guild-terminal-role-missing-original", "guild_trial_launch_role", &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 2}},
		{"guild-private-lobby-missing-original", "guild_trial_lobby_slot", &pb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, db, _, _ := guildRoleTiDB(t)
			insertPendingFixture(t, ctx, db, test.table, test.row)
			requirePending(t, ctx, db, test.table)
			_, err := db.ExecContext(ctx, "DELETE FROM `"+test.table+"` WHERE player_id=11")
			require.NoError(t, err)
			requirePending(t, ctx, db, "")
		})
	}
}

// 必须离帮只对过户成立,删角原来就不要求离帮。
func TestTiDBPendingGuildMembershipBlocksFreezeOnly(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	seedMemberRow(t, ctx, db, 44, 11, 1)
	blocker, err := runPending(t, ctx, db, pendingListing)
	require.NoError(t, err)
	require.Equal(t, "guild_member", blocker)
	blocker, err = runPending(t, ctx, db, pendingRemoval)
	require.NoError(t, err)
	require.Empty(t, blocker)
}

func TestTiDBPendingGuildKnownTerminalsDoNotBlock(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	for index, status := range []uint32{2, 3, 4, 6} {
		seq := uint64(index + 1)
		insertPendingFixture(t, ctx, db, "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 200 + seq, PlayerId: 11, Seq: seq, Status: pb.GuildAssetOpStatus(status)})
	}
	insertPendingFixture(t, ctx, db, "guild_trial_launch_role", &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 2})
	insertPendingFixture(t, ctx, db, "guild_trial_launch", &pb.GuildTrialLaunchRecord{RequestId: 51, State: 3})
	insertPendingFixture(t, ctx, db, "guild_trial_lobby", &pb.GuildTrialLobbyRecord{LobbyId: 51, State: 4})
	insertPendingFixture(t, ctx, db, "guild_trial_lobby_slot", &pb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51})
	requirePending(t, ctx, db, "")
}

// 已结清的占位墓碑:lobby_id=0 只剩冷却,不算挂起。
func TestTiDBPendingGuildSlotTombstoneDoesNotBlock(t *testing.T) {
	ctx, db, _, _ := guildRoleTiDB(t)
	insertPendingFixture(t, ctx, db, "guild_trial_lobby_slot", &pb.GuildTrialLobbySlotRecord{PlayerId: 11, CooldownUntilMs: ^uint64(0) >> 1})
	requirePending(t, ctx, db, "")
}

func TestTiDBPendingGuildLaunchTerminalRequiresOriginalAndProjectionTogether(t *testing.T) {
	for _, test := range []struct {
		name, blocker string
		rows          map[string]proto.Message
	}{
		{"trial-original-active", "guild_trial_launch_role", map[string]proto.Message{
			"guild_trial_launch":      &pb.GuildTrialLaunchRecord{RequestId: 51, State: 1},
			"guild_trial_launch_role": &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 2},
		}},
		{"trial-projection-active", "guild_trial_launch_role", map[string]proto.Message{
			"guild_trial_launch":      &pb.GuildTrialLaunchRecord{RequestId: 51, State: 3},
			"guild_trial_launch_role": &pb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 1},
		}},
		{"private-lobby-awaiting-consent", "guild_trial_lobby_slot", map[string]proto.Message{
			"guild_trial_lobby":      &pb.GuildTrialLobbyRecord{LobbyId: 51, State: 1},
			"guild_trial_lobby_slot": &pb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
		{"private-lobby-launching", "guild_trial_lobby_slot", map[string]proto.Message{
			"guild_trial_lobby":      &pb.GuildTrialLobbyRecord{LobbyId: 51, State: 2},
			"guild_trial_lobby_slot": &pb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
		{"private-lobby-unknown", "guild_trial_lobby_slot", map[string]proto.Message{
			"guild_trial_lobby":      &pb.GuildTrialLobbyRecord{LobbyId: 51, State: 99},
			"guild_trial_lobby_slot": &pb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, db, _, _ := guildRoleTiDB(t)
			for table, row := range test.rows {
				insertPendingFixture(t, ctx, db, table, row)
			}
			requirePending(t, ctx, db, test.blocker)
		})
	}
}

// 只破坏本例随机库,模拟绕过 schema 装配后的坏值:NULL 不能当成安全终态。
func TestTiDBPendingGuildNullAssetStateRemainsBlocking(t *testing.T) {
	ctx, install, db, _, _ := guildRoleTiDBWithInstall(t)
	insertPendingFixture(t, ctx, db, "guild_asset_op", &pb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 2})
	_, err := install.ExecContext(ctx, "ALTER TABLE `guild_asset_op` MODIFY `status` INT UNSIGNED NULL")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE `guild_asset_op` SET `status`=NULL WHERE player_id=11")
	require.NoError(t, err)
	requirePending(t, ctx, db, "guild_asset_op")
}

// 读不了就回错误,参与者回 Unavailable,协调者重放,不算通过。
func TestTiDBPendingGuildQueryFailureIsNotClean(t *testing.T) {
	ctx, install, db, _, _ := guildRoleTiDBWithInstall(t)
	_, err := install.ExecContext(ctx, "DROP TABLE `guild_trial_lobby_slot`")
	require.NoError(t, err)
	blocker, err := runPending(t, ctx, db, pendingListing)
	require.Error(t, err)
	require.Empty(t, blocker)
}

// 规格 §3.5 的串行点:冻结和入帮只能有一方先赢。入帮先赢则挂起判定读到成员行并回滚,冻结先赢则入帮在副本行上被拒。
func TestTiDBPendingGuildFreezeAndMembershipCommitAreMutuallyExclusive(t *testing.T) {
	const secret = "fixture-only-coordinator-secret-0000"
	ctx, db, repo, _ := guildRoleTiDB(t)
	participant, err := rolefence.NewParticipantServer(db, RoleFencePending, secret)
	require.NoError(t, err)
	for n := uint64(1); n <= 12; n++ {
		player := 100 + n
		account := fmt.Sprint("seller-", player)
		fencetest.Seed(t, ctx, db, player, account, 1)
		_, err := db.ExecContext(ctx, "INSERT INTO guild_player_state(player_id,updated_ms) VALUES (?,1)", player)
		require.NoError(t, err)
		in := &rfpb.PrepareRoleFreezeRequest{State: &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: 1, FreezeToken: 1000 + player, Version: 2, HomeZone: 1}}
		require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, rolefence.CoordinatorCaller, secret, uint64(time.Now().UnixMilli())))
		start := make(chan struct{})
		var wg sync.WaitGroup
		var out *rfpb.PrepareRoleFreezeResponse
		var prepareErr, joinErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			out, prepareErr = participant.PrepareRoleFreeze(ctx, in)
		}()
		go func() {
			defer wg.Done()
			<-start
			joinErr = repo.inTx(withRoleTargets(ctx, player), opReview, func(ctx context.Context, tx *sql.Tx) error {
				var state uint64
				if err := tx.QueryRowContext(ctx, "SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", player).Scan(&state); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, "INSERT INTO guild_member(guild_id,player_id,role,join_time_ms,last_active_ms,contribution_total,contribution_balance) VALUES (?,?,1,1,1,0,0)", 2000+player, player)
				return err
			})
		}()
		close(start)
		wg.Wait()
		require.NoError(t, prepareErr)
		require.True(t, out.Clean != (joinErr == nil), "冻结和入帮只能有一方提交: clean=%v join=%v", out.Clean, joinErr)
		var members int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member WHERE player_id=?", player).Scan(&members))
		fence, err := rolefence.Read(ctx, db, player)
		require.NoError(t, err)
		require.False(t, members > 0 && fence.FreezeToken != 0)
	}
}
