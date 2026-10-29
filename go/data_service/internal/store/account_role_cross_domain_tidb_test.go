package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	guildpb "proto/guild"
	mailpb "proto/mail"
	matchpb "proto/match"
	inboxpb "proto/player_inbox"
	teampb "proto/team"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/inboxshard"

	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func crossRoleGuardStore(t *testing.T) (*AccountDirectoryStore, *AccountRoleTradeStore, *sql.DB) {
	t.Helper()
	d, db := accountDirectoryTiDB(t)
	var schema string
	require.NoError(t, db.QueryRow("SELECT DATABASE()").Scan(&schema))
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(db, schema))
	for _, row := range []proto.Message{&guildpb.GuildPlayerStateRecord{}, &guildpb.GuildMemberRecord{}, &guildpb.GuildAssetOpRecord{}, &guildpb.GuildTrialRewardOwedRecord{}, &guildpb.GuildTrialLaunchRoleRecord{}, &guildpb.GuildTrialLaunchRecord{}, &guildpb.GuildTrialLobbyRecord{}, &guildpb.GuildTrialLobbySlotRecord{}, &mailpb.MailAssetOpRecord{}, &tradepb.TradeCommerceRecord{}, &tradepb.TradeAssetOpRecord{}, &matchpb.MatchAuthorityTicketRecord{}, &matchpb.MatchChallengeInvitationRecord{}, &teampb.TeamMembershipRecord{}, &teampb.TeamInviteIndexRecord{}, &teampb.TeamApplicationIndexRecord{}, &teampb.TeamLaunchRecord{}, &teampb.TeamLaunchRoleRecord{}, &matchpb.MatchSpectatorRecord{}, &inboxpb.PlayerInboxItemRow{}} {
		model.RegisterTable(row)
		require.NoError(t, model.CreateOrUpdateTable(row))
	}
	s := NewAccountRoleTradeStore(d, nil)
	require.NoError(t, s.ConfigureGuildExit(true, schema))
	require.NoError(t, s.ConfigureAssetContracts(schema, schema, schema, schema, schema))
	return d, s, db
}

func TestTiDBRoleFreezeAndMembershipCommitAreMutuallyExclusive(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 这里运行实际Begin与guild提交锁序的SQL接缝；不冒充在线guild RPC或原生卸载验收。
	for n := uint64(1); n <= 12; n++ {
		player := 100 + n
		account := fmt.Sprint("seller-", player)
		_, err := createInitialRoleForTest(d, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: player})
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "INSERT INTO guild_player_state(player_id,updated_ms) VALUES (?,1)", player)
		require.NoError(t, err)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var joinErr, freezeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, freezeErr = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 1000 + player, PlayerId: player, SellerAccount: account, ExpectedOwnerRevision: 1})
		}()
		go func() {
			defer wg.Done()
			<-start
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				joinErr = err
				return
			}
			defer tx.Rollback()
			var listing, state uint64
			if err = tx.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", player).Scan(&listing); err != nil {
				joinErr = err
				return
			}
			if listing != 0 {
				joinErr = accountdirectory.ErrConflict
				return
			}
			if err = tx.QueryRowContext(ctx, "SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE", player).Scan(&state); err != nil {
				joinErr = err
				return
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO guild_member(guild_id,player_id,role,join_time_ms,last_active_ms,contribution_total,contribution_balance) VALUES (?,?,1,1,1,0,0)", 2000+player, player)
			if err != nil {
				joinErr = err
				return
			}
			joinErr = tx.Commit()
		}()
		close(start)
		wg.Wait()
		require.True(t, (freezeErr == nil) != (joinErr == nil), "冻结和入帮只能有一方提交：freeze=%v join=%v", freezeErr, joinErr)
		var members int
		var listing uint64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member WHERE player_id=?", player).Scan(&members))
		require.NoError(t, db.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=?", player).Scan(&listing))
		require.False(t, members > 0 && listing != 0)
	}
}

func TestTiDBRolePendingAssetsRejectFreezeRemovalAndImport(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	w, _ := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "rmb_listed", Rmb: &tradepb.TradeRmbConsignmentWorkflow{SellerAccount: "seller", SellerOwnerRevision: 1, AssetKind: "ITEMS"}})
	_, err = db.ExecContext(ctx, "INSERT INTO trade_commerce(commerce_id,request_key,request_digest,kind,status,seller_player_id,buyer_player_id,market_id,item_config_id,side,unit_price,quantity,filled_quantity,currency_type,expires_ms,created_ms,updated_ms,version,workflow_proto,parent_id,counterparty_id) VALUES (77,'pending',?,6,1,11,0,0,0,0,1,1,0,0,0,1,1,1,?,0,0)", make([]byte, 32), w)
	require.NoError(t, err)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(51))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 1, owned.OwnerRevision)
	require.Zero(t, owned.TradeListingId)
	_, err = db.ExecContext(ctx, "UPDATE trade_commerce SET status=7 WHERE commerce_id=77")
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}))
}

func TestTiDBRoleCancelBeforeUnknownBeginPersistsPermanentTombstone(t *testing.T) {
	d, s, _ := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	for listing := uint64(501); listing < 509; listing++ {
		begin := &dspb.BeginAccountRoleTradeRequest{ListingId: listing, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
		stop := &dspb.CancelAccountRoleTradeRequest{ListingId: listing, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
		var wg sync.WaitGroup
		var beginErr, stopErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, beginErr = s.Begin(ctx, begin) }()
		go func() { defer wg.Done(); <-start; _, stopErr = s.Cancel(ctx, stop) }()
		close(start)
		wg.Wait()
		require.NoError(t, stopErr)
		require.True(t, beginErr == nil || errors.Is(beginErr, accountdirectory.ErrConflict))
		replayed, err := s.Begin(ctx, begin)
		require.NoError(t, err)
		require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, replayed.State)
		owned, err := d.Lookup(ctx, 11)
		require.NoError(t, err)
		require.Zero(t, owned.TradeListingId)
	}
}

// 夹具按正式 PB 列全集写入，不另建一份资产状态/表定义。
func insertRoleGuardFixture(t *testing.T, db *sql.DB, table string, row proto.Message) {
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
			t.Fatalf("资产guard夹具遇到未支持字段: %s", f.FullName())
		}
	}
	_, err := db.Exec("INSERT INTO `"+table+"` ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(marks, ",")+")", values...)
	require.NoError(t, err)
}

func TestTiDBRoleGuildMailPendingManualAndOwedBlockOwnershipChanges(t *testing.T) {
	for _, test := range []struct {
		name, table string
		row         proto.Message
	}{
		{"trade-pending", "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 1}},
		{"trade-manual", "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 5}},
		{"trade-unspecified", "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 0}},
		{"trade-unknown", "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 99}},
		{"guild-pending", "guild_asset_op", &guildpb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 1}},
		{"guild-manual", "guild_asset_op", &guildpb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 5}},
		{"guild-unspecified", "guild_asset_op", &guildpb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 0}},
		{"guild-unknown", "guild_asset_op", &guildpb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 99}},
		{"mail-pending", "mail_asset_op", &mailpb.MailAssetOpRecord{OpId: 51, PlayerId: 11, Status: 1}},
		{"mail-manual", "mail_asset_op", &mailpb.MailAssetOpRecord{OpId: 51, PlayerId: 11, Status: 4}},
		{"mail-unspecified", "mail_asset_op", &mailpb.MailAssetOpRecord{OpId: 51, PlayerId: 11, Status: 0}},
		{"mail-unknown", "mail_asset_op", &mailpb.MailAssetOpRecord{OpId: 51, PlayerId: 11, Status: 99}},
		{"guild-owed-after-leave", "guild_trial_reward_owed", &guildpb.GuildTrialRewardOwedRecord{PlayerId: 11, BattleId: 51}},
		{"guild-prepared-launch", "guild_trial_launch_role", &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 1}},
		{"guild-unknown-launch", "guild_trial_launch_role", &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 99}},
		{"match-queued", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", State: 1}},
		{"match-formed", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", FormationId: 51, State: 2}},
		{"match-created", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", FormationId: 51, State: 3}},
		{"match-unknown", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, State: 99}},
		{"match-state-zero-with-ticket", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original"}},
		{"match-state-zero-with-request", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, RequestId: "original"}},
		{"match-state-zero-with-formation", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, FormationId: 51}},
		{"match-state-zero-with-mode", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Mode: 1}},
		{"match-state-zero-with-config", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, ConfigId: 1}},
		{"match-state-zero-with-team-size", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, TeamSize: 1}},
		{"match-state-zero-with-enqueue", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, EnqueuedMs: 1}},
		{"match-state-zero-with-not-before", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, NotBeforeMs: 1}},
		{"match-state-zero-with-home", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, ZoneId: 1}},
		{"match-state-zero-with-rating", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, RatingCenti: -1}},
		{"match-state-zero-with-team", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, TeamId: 1}},
		{"match-state-zero-with-network", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, NetworkProfile: []byte{1}}},
		{"match-state-zero-with-expiry", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, ExpiresMs: 1}},
		{"match-state-zero-with-selected-region", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, SelectedRegion: 1}},
		{"match-state-zero-with-preferred-region", "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, PreferredRegion: 1}},
		{"match-challenge-challenger-pending", "match_challenge_invitation", &matchpb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 1, ExpiresMs: ^uint64(0)}},
		{"match-challenge-target-pending", "match_challenge_invitation", &matchpb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 12, TargetPlayerId: 11, State: 1, ExpiresMs: ^uint64(0)}},
		{"match-challenge-state-zero", "match_challenge_invitation", &matchpb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 0}},
		{"match-challenge-unknown", "match_challenge_invitation", &matchpb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 99}},
		{"team-member", "team_membership", &teampb.TeamMembershipRecord{PlayerId: 11, TeamId: 51, Epoch: 2}},
		{"team-invite", "team_invite_index", &teampb.TeamInviteIndexRecord{InviteeId: 11, TeamId: 51, ExpiresMs: ^uint64(0)}},
		{"team-application", "team_application_index", &teampb.TeamApplicationIndexRecord{ApplicantId: 11, TeamId: 51, ExpiresMs: ^uint64(0)}},
		{"team-launch-missing-original", "team_launch_role", &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 12, RequestHigh: 51, State: 5}},
		{"guild-terminal-role-missing-original", "guild_trial_launch_role", &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 2}},
		{"guild-private-lobby-missing-original", "guild_trial_lobby_slot", &guildpb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51}},
		{"inbox-pending", "player_inbox_item", &inboxpb.PlayerInboxItemRow{Shard: inboxshard.RowShard(11), PlayerId: 11, State: 1}},
		{"inbox-unspecified", "player_inbox_item", &inboxpb.PlayerInboxItemRow{Shard: inboxshard.RowShard(11), PlayerId: 11, State: 0}},
		{"inbox-unknown", "player_inbox_item", &inboxpb.PlayerInboxItemRow{Shard: inboxshard.RowShard(11), PlayerId: 11, State: 99}},
		{"spectator-pending", "match_spectator", &matchpb.MatchSpectatorRecord{PlayerId: 11, State: 1}},
		{"spectator-active", "match_spectator", &matchpb.MatchSpectatorRecord{PlayerId: 11, State: 2}},
		{"spectator-cancelling", "match_spectator", &matchpb.MatchSpectatorRecord{PlayerId: 11, State: 3}},
		{"spectator-unknown-result", "match_spectator", &matchpb.MatchSpectatorRecord{PlayerId: 11, State: 4}},
		{"spectator-invalid-state", "match_spectator", &matchpb.MatchSpectatorRecord{PlayerId: 11, State: 99}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, s, db := crossRoleGuardStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
			_, err := createInitialRoleForTest(d, ctx, "seller", role)
			require.NoError(t, err)
			insertRoleGuardFixture(t, db, test.table, test.row)
			_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
			_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(52))
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			predicate := "player_id=11"
			if test.table == "match_challenge_invitation" {
				predicate = "challenger_player_id=11 OR target_player_id=11"
			}
			if test.table == "team_invite_index" {
				predicate = "invitee_id=11"
			}
			if test.table == "team_application_index" {
				predicate = "applicant_id=11"
			}
			_, err = db.Exec("DELETE FROM `" + test.table + "` WHERE " + predicate)
			require.NoError(t, err)
			_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.NoError(t, err)
		})
	}
}

func TestTiDBRoleKnownAssetTerminalsAndMatchTerminalAllowOwnershipChanges(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	for index, status := range []uint32{2, 3, 4, 6} {
		seq := uint64(index + 1)
		insertRoleGuardFixture(t, db, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 100 + seq, PlayerId: 11, Seq: seq, Status: tradepb.TradeAssetOpStatus(status)})
		insertRoleGuardFixture(t, db, "guild_asset_op", &guildpb.GuildAssetOpRecord{OpId: 200 + seq, PlayerId: 11, Seq: seq, Status: guildpb.GuildAssetOpStatus(status)})
	}
	for index, status := range []uint32{2, 3, 5} {
		seq := uint64(index + 1)
		insertRoleGuardFixture(t, db, "mail_asset_op", &mailpb.MailAssetOpRecord{OpId: 300 + seq, PlayerId: 11, Seq: seq, Status: status})
	}
	insertRoleGuardFixture(t, db, "guild_trial_launch_role", &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, Account: "seller", OwnerRevision: 1, State: 2})
	insertRoleGuardFixture(t, db, "guild_trial_launch", &guildpb.GuildTrialLaunchRecord{RequestId: 51, State: 3})
	insertRoleGuardFixture(t, db, "guild_trial_lobby", &guildpb.GuildTrialLobbyRecord{LobbyId: 51, State: 4})
	insertRoleGuardFixture(t, db, "guild_trial_lobby_slot", &guildpb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51})
	insertRoleGuardFixture(t, db, "team_launch", &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 5})
	insertRoleGuardFixture(t, db, "team_launch_role", &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 5})
	for _, status := range []uint32{2, 3, 4, 5} {
		insertRoleGuardFixture(t, db, "player_inbox_item", &inboxpb.PlayerInboxItemRow{Shard: inboxshard.RowShard(11), PlayerId: 11, Seq: uint64(status), RequestId: uint64(status), State: status, OwnerAccount: "seller", OwnerRevision: 1})
	}
	insertRoleGuardFixture(t, db, "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "finished", FormationId: 51, State: 4})
	for index, state := range []uint32{1, 2, 3} {
		expires := ^uint64(0)
		if state == 1 {
			expires = 1
		}
		insertRoleGuardFixture(t, db, "match_challenge_invitation", &matchpb.MatchChallengeInvitationRecord{ChallengeId: uint64(51 + index), ChallengerPlayerId: 11, TargetPlayerId: 12, State: state, ExpiresMs: expires})
	}
	_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(53))
	require.NoError(t, err)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: owned.OwnerRevision}}))
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(54))
	require.NoError(t, err)
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: owned.OwnerRevision})
	require.NoError(t, err)
}

func TestTiDBRoleExactEmptyMatchPlaceholderDoesNotTrapFreeze(t *testing.T) {
	for _, account := range []string{"", "seller"} {
		t.Run("placeholder-owner-"+account, func(t *testing.T) {
			d, s, db := crossRoleGuardStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
			require.NoError(t, err)
			insertRoleGuardFixture(t, db, "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, OwnerAccount: account, OwnerRevision: 1})
			_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.NoError(t, err)
		})
	}
}

func TestTiDBRoleLaunchTerminalRequiresOriginalAndProjectionTogether(t *testing.T) {
	for _, test := range []struct {
		name string
		rows map[string]proto.Message
	}{
		{"team-original-active", map[string]proto.Message{
			"team_launch":      &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 1},
			"team_launch_role": &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 5},
		}},
		{"team-projection-active", map[string]proto.Message{
			"team_launch":      &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 5},
			"team_launch_role": &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 1},
		}},
		{"trial-original-active", map[string]proto.Message{
			"guild_trial_launch":      &guildpb.GuildTrialLaunchRecord{RequestId: 51, State: 1},
			"guild_trial_launch_role": &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 2},
		}},
		{"trial-projection-active", map[string]proto.Message{
			"guild_trial_launch":      &guildpb.GuildTrialLaunchRecord{RequestId: 51, State: 3},
			"guild_trial_launch_role": &guildpb.GuildTrialLaunchRoleRecord{PlayerId: 11, RequestId: 51, State: 1},
		}},
		{"private-lobby-awaiting-consent", map[string]proto.Message{
			"guild_trial_lobby":      &guildpb.GuildTrialLobbyRecord{LobbyId: 51, State: 1},
			"guild_trial_lobby_slot": &guildpb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
		{"private-lobby-launching", map[string]proto.Message{
			"guild_trial_lobby":      &guildpb.GuildTrialLobbyRecord{LobbyId: 51, State: 2},
			"guild_trial_lobby_slot": &guildpb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
		{"private-lobby-unknown", map[string]proto.Message{
			"guild_trial_lobby":      &guildpb.GuildTrialLobbyRecord{LobbyId: 51, State: 99},
			"guild_trial_lobby_slot": &guildpb.GuildTrialLobbySlotRecord{PlayerId: 11, LobbyId: 51},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, s, db := crossRoleGuardStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
			_, err := createInitialRoleForTest(d, ctx, "seller", role)
			require.NoError(t, err)
			for table, row := range test.rows {
				insertRoleGuardFixture(t, db, table, row)
			}
			_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
			_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(58))
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			owned, err := d.Lookup(ctx, 11)
			require.NoError(t, err)
			require.EqualValues(t, 1, owned.OwnerRevision)
			require.Zero(t, owned.TradeListingId)
		})
	}
}

func TestTiDBRolePendingGuardMissingSchemaOrColumnDisablesAllOwnershipChanges(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	var schema string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema))
	for _, matchSchema := range []string{"", "bad.schema", "missing_role_match_guard_schema"} {
		require.Error(t, s.ConfigureAssetContracts(schema, schema, schema, matchSchema, schema))
		require.Nil(t, d.contracts)
		require.Nil(t, s.contracts)
		_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
		require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
		require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrUnavailable)
		_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(55))
		require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	}
	for _, inboxSchema := range []string{"", "bad.schema", "missing_role_inbox_guard_schema"} {
		require.Error(t, s.ConfigureAssetContracts(schema, schema, schema, schema, inboxSchema))
		require.Nil(t, d.contracts)
		require.Nil(t, s.contracts)
		_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
		require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	}
	require.NoError(t, s.ConfigureAssetContracts(schema, schema, schema, schema, schema))
	_, err = db.ExecContext(ctx, "ALTER TABLE match_authority_ticket DROP COLUMN ticket")
	require.NoError(t, err)
	require.Error(t, s.ConfigureAssetContracts(schema, schema, schema, schema, schema))
	require.Nil(t, d.contracts)
	require.Nil(t, s.contracts)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 1, owned.OwnerRevision)
	require.Zero(t, owned.TradeListingId)
}

func TestTiDBRoleMatchParticipationAndFreezeCommitSharePlayerLock(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for player := uint64(51); player <= 56; player++ {
		account := fmt.Sprint("seller-", player)
		_, err := createInitialRoleForTest(d, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: player})
		require.NoError(t, err)
		insertRoleGuardFixture(t, db, "match_authority_ticket", &matchpb.MatchAuthorityTicketRecord{PlayerId: player})
		start := make(chan struct{})
		var wg sync.WaitGroup
		var freezeErr, matchErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, freezeErr = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 1000 + player, PlayerId: player, SellerAccount: account, ExpectedOwnerRevision: 1})
		}()
		go func() {
			defer wg.Done()
			<-start
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				matchErr = err
				return
			}
			defer tx.Rollback()
			var listing uint64
			if err := tx.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", player).Scan(&listing); err != nil {
				matchErr = err
				return
			}
			if listing != 0 {
				matchErr = accountdirectory.ErrConflict
				return
			}
			if _, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=1,ticket='fixture-original',request_id='fixture-original',owner_account=?,owner_revision=1 WHERE player_id=?", account, player); err != nil {
				matchErr = err
				return
			}
			matchErr = tx.Commit()
		}()
		close(start)
		wg.Wait()
		require.True(t, (freezeErr == nil) != (matchErr == nil), "冻结与新匹配参与只能一方提交: freeze=%v match=%v", freezeErr, matchErr)
		var listing, state uint64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=?", player).Scan(&listing))
		require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM match_authority_ticket WHERE player_id=?", player).Scan(&state))
		require.False(t, listing != 0 && state == 1)
	}
}

func TestTiDBRoleUnknownCommerceStatusCannotBorrowKnownRefundStage(t *testing.T) {
	d, s, db := crossRoleGuardStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "official_refund"})
	require.NoError(t, err)
	insertRoleGuardFixture(t, db, "trade_commerce", &tradepb.TradeCommerceRecord{CommerceId: 77, RequestKey: "fixture-refund", Kind: 1, SellerPlayerId: 11, Status: 99, WorkflowProto: raw})
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(56))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = db.ExecContext(ctx, "UPDATE trade_commerce SET status=8 WHERE commerce_id=77")
	require.NoError(t, err)
	insertRoleGuardFixture(t, db, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 5})
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = db.ExecContext(ctx, "UPDATE trade_asset_op SET status=3 WHERE op_id=51")
	require.NoError(t, err)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
}

func TestTiDBRoleNullAssetStateRemainsBlocking(t *testing.T) {
	for _, test := range []struct {
		table, column string
		row           proto.Message
	}{
		{"trade_asset_op", "status", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 2}},
		{"guild_asset_op", "status", &guildpb.GuildAssetOpRecord{OpId: 51, PlayerId: 11, Status: 2}},
		{"mail_asset_op", "status", &mailpb.MailAssetOpRecord{OpId: 51, PlayerId: 11, Status: 2}},
		{"match_authority_ticket", "state", &matchpb.MatchAuthorityTicketRecord{PlayerId: 11, State: 4}},
		{"match_challenge_invitation", "state", &matchpb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 3}},
	} {
		t.Run(test.table, func(t *testing.T) {
			d, s, db := crossRoleGuardStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}
			_, err := createInitialRoleForTest(d, ctx, "seller", role)
			require.NoError(t, err)
			insertRoleGuardFixture(t, db, test.table, test.row)
			// 只破坏本例随机库，模拟绕过schema装配后的坏值；不能把NULL当成安全终态。
			_, err = db.ExecContext(ctx, "ALTER TABLE `"+test.table+"` MODIFY `"+test.column+"` INT UNSIGNED NULL")
			require.NoError(t, err)
			predicate := "player_id=11"
			if test.table == "match_challenge_invitation" {
				predicate = "challenger_player_id=11 OR target_player_id=11"
			}
			_, err = db.ExecContext(ctx, "UPDATE `"+test.table+"` SET `"+test.column+"`=NULL WHERE "+predicate)
			require.NoError(t, err)
			_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 88, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
			_, err = d.ImportRole(ctx, "seller", role, false, roleImportTestID(57))
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
		})
	}
}
