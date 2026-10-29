package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	guildpb "proto/guild"
	pb "proto/match"
	teampb "proto/team"
)

var ErrMatchEntryOccupied = errors.New("角色已有未终结的队伍或历练原意图")

// 匹配票占用按票状态细分，均包装 ErrMatchEntryOccupied：既有 errors.Is 判断不变，
// 观战入口据此区分「匹配中」与「战斗中」两种客户端结论。
var (
	// ErrTicketQueued：票在排队或成局中；空占位以外的 state0 与未知状态同样按此处理。
	ErrTicketQueued = fmt.Errorf("%w: 匹配票排队或成局中", ErrMatchEntryOccupied)
	// ErrTicketInBattle：票已建房（state3）。
	ErrTicketInBattle = fmt.Errorf("%w: 匹配票已建房", ErrMatchEntryOccupied)
)

// ticketOccupiedErr 只给 TicketOccupied 为真的票选细分原因。
func ticketOccupiedErr(ticket *pb.MatchAuthorityTicketRecord) error {
	if ticket.GetState() == 3 {
		return ErrTicketInBattle
	}
	return ErrTicketQueued
}

// 仅传递调用者已持有的正规原件，不复制另一套业务状态。例外仍须与本事务读到的SQL原件逐项相等。
type matchEntryOriginal struct {
	teams     []*teampb.TeamLaunchRecord
	activity  *pb.StartActivityBattleRequest
	battleID  uint64
	direct    *pb.MatchDirectIntentRecord
	payload   []byte
	formation *pb.MatchFormationRecord
}

// 配置只在启动装配；所有跨库读取复用当前TiDB主池，缺表、缺列或缺schema拒绝接流。
func (s *Store) ConfigureMatchEntryAuthority(ctx context.Context, schema string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(schema) {
		return errors.New("全球匹配须显式配置GuildTrialSchema")
	}
	for _, query := range []string{
		"SELECT request_id,battle_id,state,original_proto,request_digest FROM `" + schema + "`.guild_trial_launch LIMIT 0",
		"SELECT player_id,request_id,account,owner_revision,state FROM `" + schema + "`.guild_trial_launch_role LIMIT 0",
		"SELECT player_id,lobby_id,cooldown_until_ms FROM `" + schema + "`.guild_trial_lobby_slot LIMIT 0",
		"SELECT lobby_id,guild_id,activity_id,initiator_player_id,state,view_proto,original_proto,consent_proto,battle_id FROM `" + schema + "`.guild_trial_lobby LIMIT 0",
	} {
		rows, err := s.DB.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	s.entryGuildSchema = schema
	s.entryGuard = s.requireEntryOriginalsTx
	return nil
}

// 调用者必须已经一次持有完整参与者P；本接缝不追加P，也不在Team/Guild业务锁之前反向锁别域。
// 这些占用的全部写者同样持P，因此RC主池的当前读与首次提交互斥，TTL不参与授权。
func (s *Store) requireMatchEntryTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord, source *matchEntryOriginal) error {
	if s.entryGuard == nil || len(group) == 0 {
		return ErrMatchEntryOccupied
	}
	return s.entryGuard(ctx, tx, group, source)
}

// Team首次launch尚未建票，必须先在同一全P事务核真实票及其他已提交launch。
// 观战由原业务的一次性退出原件处理，此处不以当前watch替换那份原件。
func (s *Store) RequireNewTeamLaunchTx(ctx context.Context, tx *sql.Tx, originals []*teampb.TeamOriginalRole) error {
	group := make([]*pb.MatchAuthorityTicketRecord, 0, len(originals))
	for _, original := range originals {
		group = append(group, &pb.MatchAuthorityTicketRecord{PlayerId: original.GetPlayerId(), OwnerAccount: original.GetOwnerAccount(), OwnerRevision: original.GetOwnerRevision()})
	}
	if err := s.requireMatchEntryTx(ctx, tx, group, nil); err != nil {
		return err
	}
	return s.requireIdleMatchTicketsTx(ctx, tx, group)
}

func (s *Store) requireIdleMatchTicketsTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord) error {
	group = append([]*pb.MatchAuthorityTicketRecord(nil), group...)
	sort.Slice(group, func(i, j int) bool { return group[i].PlayerId < group[j].PlayerId })
	for _, member := range group {
		ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", member.PlayerId))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if TicketOccupied(ticket) {
			return ticketOccupiedErr(ticket)
		}
	}
	return nil
}

func directEntryOriginal(key *pb.MatchDirectIntentRecord, payload []byte, formation *pb.MatchFormationRecord) (*matchEntryOriginal, error) {
	if key.Kind != 2 {
		return nil, nil
	}
	request := &pb.StartActivityBattleRequest{}
	if proto.Unmarshal(payload, request) != nil || request.RequestId == 0 || request.RequestId != key.RequestLow || key.RequestHigh != 0 || request.GetActivityContext().GetGuildId() != key.OwnerId || request.GetActivityContext().GetLaunchRequestId() != request.RequestId || request.GetActivityContext().GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL || request.BattleConfigId != formation.ConfigId {
		return nil, ErrDirectConflict
	}
	var activity battlepb.BattleActivityContext
	if proto.Unmarshal(formation.ActivityContext, &activity) != nil || !proto.Equal(&activity, request.ActivityContext) {
		return nil, ErrDirectConflict
	}
	return &matchEntryOriginal{activity: request, battleID: formation.BattleId}, nil
}

func (s *Store) requireEntryOriginalsTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord, source *matchEntryOriginal) error {
	if source != nil && source.direct != nil {
		parsed, err := directEntryOriginal(source.direct, source.payload, source.formation)
		if err != nil {
			return err
		}
		if parsed != nil {
			copy := *source
			copy.activity = parsed.activity
			source = &copy
		}
	}
	byPlayer := make(map[uint64]*pb.MatchAuthorityTicketRecord, len(group))
	for _, member := range group {
		if member == nil || member.PlayerId == 0 || byPlayer[member.PlayerId] != nil {
			return ErrOriginalPlayer
		}
		byPlayer[member.PlayerId] = member
	}
	ownTeams := map[[3]uint64]*teampb.TeamLaunchRecord{}
	requiredTeam := map[uint64][3]uint64{}
	if source != nil {
		for _, expected := range source.teams {
			data, err := DecodeTeamLaunch(expected)
			if err != nil {
				return err
			}
			for _, member := range data.Members {
				actual := byPlayer[member.Original.PlayerId]
				if actual == nil || actual.OwnerAccount != member.Original.OwnerAccount || actual.OwnerRevision != member.Original.OwnerRevision || actual.TeamId != expected.TeamId || actual.Ticket != member.Ticket {
					return ErrOriginalPlayer
				}
				if _, duplicate := requiredTeam[member.Original.PlayerId]; duplicate {
					return ErrMatchEntryOccupied
				}
				requiredTeam[member.Original.PlayerId] = [3]uint64{expected.ActorId, expected.RequestHigh, expected.RequestLow}
			}
			ownTeams[[3]uint64{expected.ActorId, expected.RequestHigh, expected.RequestLow}] = expected
		}
		if source.activity != nil {
			if err := s.requireOriginalActivityTx(ctx, tx, source.activity, byPlayer); err != nil {
				return err
			}
		}
	}
	ordered := append([]*pb.MatchAuthorityTicketRecord(nil), group...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	for _, member := range ordered {
		if member.TeamId != 0 {
			if _, own := requiredTeam[member.PlayerId]; !own {
				return ErrMatchEntryOccupied
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT r.actor_id,r.request_high,r.request_low,r.state FROM team_launch_role r LEFT JOIN team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low WHERE r.player_id=? AND (r.state IS NULL OR r.state<>5 OR l.state IS NULL OR l.state<>5) ORDER BY r.state,r.actor_id,r.request_high,r.request_low LIMIT 2", member.PlayerId)
		if err != nil {
			return err
		}
		var keys [][4]uint64
		for rows.Next() {
			var key [4]uint64
			if err = rows.Scan(&key[0], &key[1], &key[2], &key[3]); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(keys) > 1 {
			return ErrMatchEntryOccupied
		}
		if required, own := requiredTeam[member.PlayerId]; own && (len(keys) != 1 || required != [3]uint64{keys[0][0], keys[0][1], keys[0][2]}) {
			return ErrMatchEntryOccupied
		}
		for _, key := range keys {
			expected := ownTeams[[3]uint64{key[0], key[1], key[2]}]
			if expected == nil || key[3] < 1 || key[3] > 3 {
				return ErrMatchEntryOccupied
			}
			actual, err := scanTeamLaunch(tx.QueryRowContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=?", key[0], key[1], key[2]))
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrMatchEntryOccupied
				}
				return err
			}
			if actual.State != uint32(key[3]) || actual.State < 1 || actual.State > 3 || actual.TeamId != expected.TeamId || actual.OwnerAccount != expected.OwnerAccount || actual.OwnerRevision != expected.OwnerRevision || !bytes.Equal(actual.Payload, expected.Payload) || (actual.BattleId != 0 && actual.BattleId != source.battleID) {
				return ErrMatchEntryOccupied
			}
			if _, err = DecodeTeamLaunch(actual); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, "SELECT r.request_id,r.account,r.owner_revision,r.state FROM `"+s.entryGuildSchema+"`.guild_trial_launch_role r LEFT JOIN `"+s.entryGuildSchema+"`.guild_trial_launch l ON l.request_id=r.request_id WHERE r.player_id=? AND (r.state IS NULL OR r.state<>2 OR l.state IS NULL OR l.state<>3) ORDER BY r.state,r.request_id LIMIT 2", member.PlayerId)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var id, revision uint64
			var account string
			var state uint32
			if err = rows.Scan(&id, &account, &revision, &state); err != nil {
				rows.Close()
				return err
			}
			count++
			if count > 1 || state != 1 || source == nil || source.activity == nil || source.activity.RequestId != id || member.OwnerAccount != account || member.OwnerRevision != revision {
				rows.Close()
				return ErrMatchEntryOccupied
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if err = s.requireTrialLobbyEntryTx(ctx, tx, member, source, byPlayer); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) requireTrialLobbyEntryTx(ctx context.Context, tx *sql.Tx, member *pb.MatchAuthorityTicketRecord, source *matchEntryOriginal, players map[uint64]*pb.MatchAuthorityTicketRecord) error {
	var lobbyID uint64
	err := tx.QueryRowContext(ctx, "SELECT lobby_id FROM `"+s.entryGuildSchema+"`.guild_trial_lobby_slot WHERE player_id=?", member.PlayerId).Scan(&lobbyID)
	ownActivity := source != nil && source.activity != nil
	if errors.Is(err, sql.ErrNoRows) || (err == nil && lobbyID == 0) {
		if ownActivity {
			return ErrMatchEntryOccupied
		}
		return nil
	}
	if err != nil {
		return err
	}
	var guild, initiator, battle uint64
	var activity, state uint32
	var raw, consents, viewRaw []byte
	err = tx.QueryRowContext(ctx, "SELECT guild_id,activity_id,initiator_player_id,state,original_proto,consent_proto,battle_id,view_proto FROM `"+s.entryGuildSchema+"`.guild_trial_lobby WHERE lobby_id=?", lobbyID).Scan(&guild, &activity, &initiator, &state, &raw, &consents, &battle, &viewRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMatchEntryOccupied
	}
	if err != nil {
		return err
	}
	if state == 4 && !ownActivity {
		return nil
	}
	if !ownActivity || state != 2 || battle != 0 || lobbyID != source.activity.RequestId {
		return ErrMatchEntryOccupied
	}
	original := &guildpb.GuildTrialLobbyOriginal{}
	savedConsents := &guildpb.GuildTrialLobbyConsents{}
	view := &guildpb.GuildTrialLobbyView{}
	actx := source.activity.ActivityContext
	if len(raw) == 0 || len(raw) > 64*1024 || len(consents) == 0 || len(consents) > 64*1024 || len(viewRaw) == 0 || len(viewRaw) > 64*1024 || proto.Unmarshal(raw, original) != nil || proto.Unmarshal(consents, savedConsents) != nil || proto.Unmarshal(viewRaw, view) != nil || original.LobbyId != lobbyID || original.GuildId != guild || original.ActivityId != activity || original.InitiatorPlayerId != initiator || actx.GetGuildId() != guild || actx.GetActivityId() != activity || actx.GetInitiatorPlayerId() != initiator || len(original.Roster) != len(players) || len(original.Roster) < 2 || len(original.Roster) > 16 || len(savedConsents.Consents) != len(players) || view.LobbyId != lobbyID || view.InitiatorPlayerId != initiator || view.State != guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING || view.BattleId != 0 {
		return ErrMatchEntryOccupied
	}
	invited := map[uint64]*guildpb.GuildTrialRoleIdentity{}
	var previous uint64
	for _, role := range original.Roster {
		if role.GetPlayerId() <= previous || role.GetAccount() == "" || role.GetOwnerRevision() == 0 {
			return ErrOriginalPlayer
		}
		invited[role.PlayerId] = role
		previous = role.PlayerId
	}
	accepted := map[uint64]bool{}
	for _, consent := range savedConsents.Consents {
		role := consent.GetIdentity()
		current := players[role.GetPlayerId()]
		if consent.GetLobbyId() != lobbyID || current == nil || accepted[role.GetPlayerId()] || !proto.Equal(invited[role.GetPlayerId()], role) || current.OwnerAccount != role.GetAccount() || current.OwnerRevision != role.GetOwnerRevision() {
			return ErrOriginalPlayer
		}
		accepted[role.PlayerId] = true
	}
	if !accepted[member.PlayerId] || !accepted[initiator] {
		return ErrOriginalPlayer
	}
	for _, ids := range [][]uint64{view.MemberPlayerIds, view.AcceptedPlayerIds} {
		if len(ids) != len(players) {
			return ErrOriginalPlayer
		}
		seen := map[uint64]bool{}
		for _, player := range ids {
			if !accepted[player] || seen[player] {
				return ErrOriginalPlayer
			}
			seen[player] = true
		}
	}
	return nil
}

func (s *Store) requireOriginalActivityTx(ctx context.Context, tx *sql.Tx, request *pb.StartActivityBattleRequest, players map[uint64]*pb.MatchAuthorityTicketRecord) error {
	var raw, digest []byte
	var state uint32
	var battle uint64
	if err := tx.QueryRowContext(ctx, "SELECT state,battle_id,original_proto,request_digest FROM `"+s.entryGuildSchema+"`.guild_trial_launch WHERE request_id=?", request.RequestId).Scan(&state, &battle, &raw, &digest); err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	original := &guildpb.GuildTrialLaunchOriginal{}
	if state != 1 || battle != 0 || len(raw) == 0 || len(raw) > 64*1024 || !bytes.Equal(sum[:], digest) || proto.Unmarshal(raw, original) != nil || original.RequestId != request.RequestId || len(original.Roster) != len(players) || len(original.Roster) < 2 || len(original.Roster) > 5 {
		return ErrMatchEntryOccupied
	}
	var previous uint64
	memberIDs := []uint64{original.InitiatorPlayerId}
	initiator := false
	for _, member := range original.Roster {
		actual := players[member.GetPlayerId()]
		if member.GetPlayerId() <= previous || actual == nil || actual.OwnerAccount != member.Account || actual.OwnerRevision != member.OwnerRevision {
			return ErrOriginalPlayer
		}
		previous = member.PlayerId
		if member.PlayerId == original.InitiatorPlayerId {
			initiator = true
		} else {
			memberIDs = append(memberIDs, member.PlayerId)
		}
		var account string
		var revision uint64
		var roleState uint32
		if err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,state FROM `"+s.entryGuildSchema+"`.guild_trial_launch_role WHERE player_id=? AND request_id=?", member.PlayerId, original.RequestId).Scan(&account, &revision, &roleState); err != nil {
			return err
		}
		if roleState != 1 || account != member.Account || revision != member.OwnerRevision {
			return ErrMatchEntryOccupied
		}
	}
	expected := &pb.StartActivityBattleRequest{RequestId: original.RequestId, BattleConfigId: original.DungeonId, MemberPlayerIds: memberIDs, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: original.GuildId, ActivityId: original.ActivityId, PeriodKey: original.PeriodKey, GuildPeriodKey: original.GuildPeriodKey, InitiatorPlayerId: original.InitiatorPlayerId, LaunchRequestId: original.RequestId}}
	if !initiator || !proto.Equal(expected, request) {
		return ErrMatchEntryOccupied
	}
	return nil
}
