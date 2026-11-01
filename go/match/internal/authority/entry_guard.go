package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
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
	// admitted:活动开局的提交点已经过了,全员票已写,这里是接着成局。不再要求占用还在,见 requireRoleOccupancyTx。
	admitted bool
}

// 调用者必须已经一次持有完整参与者P；本接缝不追加P，也不在Team业务锁之前反向锁别域。
// 这些占用的全部写者同样持P，因此RC主池的当前读与首次提交互斥，TTL不参与队伍和票的授权。
// 帮会历练不再跨库读 guild 的表,读本库 match_role_occupancy(规格 §4),那一行有租约。
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
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
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
		if err = requireRoleOccupancyTx(ctx, tx, member, source, stamp); err != nil {
			return err
		}
	}
	return requireActivityRosterTx(ctx, tx, group, source)
}
