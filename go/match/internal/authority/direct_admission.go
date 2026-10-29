package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"shared/matchentry"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

const directColumns = "kind,owner_id,request_high,request_low,payload,battle_id,cancelled,owner_account,owner_revision,spectator_exits,exit_prepared,start_original"

// 只有完整空占位0或终态4可接新意图；任何未知值及非空pending0都仍占用。
func TicketOccupied(ticket *pb.MatchAuthorityTicketRecord) bool {
	return matchentry.TicketOccupied(ticket)
}
func scanDirect(row scanner) (*pb.MatchDirectIntentRecord, error) {
	out := &pb.MatchDirectIntentRecord{}
	var exits []byte
	err := row.Scan(&out.Kind, &out.OwnerId, &out.RequestHigh, &out.RequestLow, &out.Payload, &out.BattleId, &out.Cancelled, &out.OwnerAccount, &out.OwnerRevision, &exits, &out.ExitPrepared, &out.StartOriginal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wrapper := &pb.MatchDirectIntentRecord{}
	if len(exits) > 0 && proto.Unmarshal(exits, wrapper) != nil {
		return nil, ErrDirectConflict
	}
	out.SpectatorExits = wrapper.SpectatorExits
	return out, nil
}
func (s *Store) DirectAdmission(ctx context.Context, key *pb.MatchDirectIntentRecord) (*pb.MatchDirectIntentRecord, error) {
	if key == nil {
		return nil, ErrDirectConflict
	}
	return scanDirect(s.DB.QueryRowContext(ctx, "SELECT "+directColumns+" FROM match_direct_intent WHERE kind=? AND owner_id=? AND request_high=? AND request_low=?", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow))
}
func readDirectTx(ctx context.Context, tx *sql.Tx, key *pb.MatchDirectIntentRecord) (*pb.MatchDirectIntentRecord, error) {
	return scanDirect(tx.QueryRowContext(ctx, "SELECT "+directColumns+" FROM match_direct_intent WHERE kind=? AND owner_id=? AND request_high=? AND request_low=? FOR UPDATE", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow))
}
func DecodeDirectStart(record *pb.MatchDirectIntentRecord) (*pb.MatchDirectStartOriginal, error) {
	original := &pb.MatchDirectStartOriginal{}
	if record == nil || !record.ExitPrepared || proto.Unmarshal(record.StartOriginal, original) != nil || original.Formation == nil || original.Formation.BattleId == 0 || original.Formation.OperationId == "" || len(original.Tickets) == 0 || len(original.Tickets) > 10 {
		return nil, ErrDirectConflict
	}
	seen := map[uint64]*pb.MatchAuthorityTicketRecord{}
	for _, ticket := range original.Tickets {
		if ticket == nil || ticket.PlayerId == 0 || ticket.Ticket == "" || ticket.RequestId != original.Formation.OperationId || seen[ticket.PlayerId] != nil || ticket.Mode != original.Formation.Mode || ticket.ConfigId != original.Formation.ConfigId {
			return nil, ErrDirectConflict
		}
		seen[ticket.PlayerId] = ticket
	}
	for _, exit := range record.SpectatorExits {
		if validateSpectator(exit) != nil || exit.State != 3 || seen[exit.PlayerId] == nil || !sameSpectatorOwner(exit, seen[exit.PlayerId]) {
			return nil, ErrOriginalPlayer
		}
	}
	return original, nil
}
func validateDirectActor(ctx context.Context, key *pb.MatchDirectIntentRecord, group []*pb.MatchAuthorityTicketRecord) (*pb.MatchAuthorityTicketRecord, error) {
	if key.Kind == 2 {
		return &pb.MatchAuthorityTicketRecord{}, nil
	}
	actor := originalPlayer(ctx, key.OwnerId)
	for _, ticket := range group {
		if ticket.PlayerId == actor.PlayerId && ticket.OwnerAccount == actor.OwnerAccount && ticket.OwnerRevision == actor.OwnerRevision {
			return actor, nil
		}
	}
	return nil, ErrOriginalPlayer
}

// 业务首次P内固定直接开战原件、退出列表与全员pending0。网络只在提交以后发送。
func (s *Store) PrepareDirectAdmission(ctx context.Context, key *pb.MatchDirectIntentRecord, payload []byte, formation *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) (*pb.MatchDirectIntentRecord, error) {
	if key == nil || key.Kind < 1 || key.Kind > 3 || key.OwnerId == 0 || (key.RequestHigh == 0 && key.RequestLow == 0) || len(payload) == 0 {
		return nil, ErrDirectConflict
	}
	observed, err := s.DirectAdmission(ctx, key)
	if err != nil {
		return nil, err
	}
	if observed != nil && observed.ExitPrepared {
		original, err := DecodeDirectStart(observed)
		if err != nil {
			return nil, err
		}
		group = original.Tickets
	}
	if len(group) == 0 || len(group) > 10 {
		return nil, ErrDirectConflict
	}
	for _, ticket := range group {
		if ticket == nil || ticket.PlayerId == 0 {
			return nil, ErrDirectConflict
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, group); err != nil {
		return nil, err
	}
	actor, err := validateDirectActor(ctx, key, group)
	if err != nil {
		return nil, err
	}
	// TiDB 的缺行 FOR UPDATE 不提供主键占位；先在原 P 事务抢同一永久主键。
	// 只将这条 INSERT 的重复主键交给锁内原件复核，截断、锁超时等错误仍返回。
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_direct_intent(kind,owner_id,request_high,request_low,payload,battle_id,cancelled,owner_account,owner_revision) VALUES(?,?,?,?,?,0,0,?,?)", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow, payload, actor.OwnerAccount, actor.OwnerRevision); err != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			return nil, err
		}
	}
	current, err := readDirectTx(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if current == nil || !bytes.Equal(current.Payload, payload) {
		return nil, ErrDirectConflict
	}
	// 永久取消只认同正文；墓碑不需要伪造已经准备过的 actor 绑定。
	if current.Cancelled {
		return nil, ErrDirectCancelled
	}
	if current.OwnerAccount != actor.OwnerAccount || current.OwnerRevision != actor.OwnerRevision {
		return nil, ErrDirectConflict
	}
	if current.ExitPrepared || current.BattleId != 0 {
		return current, tx.Commit()
	}
	out, err := s.prepareDirectAdmissionTx(ctx, tx, key, payload, formation, group, actor, current)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
func (s *Store) prepareDirectAdmissionTx(ctx context.Context, tx *sql.Tx, key *pb.MatchDirectIntentRecord, payload []byte, formation *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord, actor *pb.MatchAuthorityTicketRecord, current *pb.MatchDirectIntentRecord) (*pb.MatchDirectIntentRecord, error) {
	if formation == nil || formation.BattleId == 0 || formation.OperationId == "" || formation.RequeueOnFailure {
		return nil, ErrDirectConflict
	}
	if err := s.requireMatchEntryTx(ctx, tx, group, &matchEntryOriginal{direct: key, payload: payload, formation: formation, battleID: formation.BattleId}); err != nil {
		return nil, err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	original := &pb.MatchDirectStartOriginal{Formation: proto.Clone(formation).(*pb.MatchFormationRecord)}
	for _, source := range group {
		ticket := proto.Clone(source).(*pb.MatchAuthorityTicketRecord)
		var raw []byte
		var expiry uint64
		if err = tx.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=?", ticket.PlayerId).Scan(&raw, &expiry); err != nil {
			return nil, err
		}
		profile, err := DecodeProfile(raw)
		if err != nil {
			return nil, err
		}
		if profile.OwnerAccount != ticket.OwnerAccount || profile.OwnerRevision != ticket.OwnerRevision {
			return nil, ErrOriginalPlayer
		}
		if expiry <= stamp || len(Latencies(profile, stamp, s.Config.HardLatencyMs)) == 0 {
			return nil, ErrDirectConflict
		}
		if _, ok := Latencies(profile, stamp, s.Config.HardLatencyMs)[formation.PhysicalRegion]; !ok {
			return nil, ErrDirectConflict
		}
		ticket.NetworkProfile, ticket.ExpiresMs, ticket.EnqueuedMs, ticket.State, ticket.FormationId, ticket.SelectedRegion = raw, expiry, stamp, 0, 0, 0
		ticket.NotBeforeMs = 0
		ticket.RequestId = formation.OperationId
		ticket.PreferredRegion = PreferredRegion(profile, stamp)
		original.Tickets = append(original.Tickets, ticket)
	}
	exits, err := s.CaptureSpectatorExitsTx(ctx, tx, original.Tickets)
	if err != nil {
		return nil, err
	}
	ordered := append([]*pb.MatchAuthorityTicketRecord(nil), original.Tickets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	for index, ticket := range ordered {
		if ticket.PlayerId == 0 || ticket.Ticket == "" || ticket.RequestId != formation.OperationId || (index > 0 && ordered[index-1].PlayerId == ticket.PlayerId) {
			return nil, ErrDirectConflict
		}
		if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", ticket.PlayerId, []byte{}); err != nil {
			return nil, err
		}
		old, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", ticket.PlayerId))
		if err != nil {
			return nil, err
		}
		if TicketOccupied(old) {
			return nil, ErrDirectConflict
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket=?,request_id=?,mode=?,config_id=?,team_size=?,state=0,enqueued_ms=?,not_before_ms=0,zone_id=?,rating_centi=?,formation_id=0,team_id=?,network_profile=?,expires_ms=?,selected_region=0,preferred_region=?,owner_account=?,owner_revision=? WHERE player_id=?", ticket.Ticket, ticket.RequestId, ticket.Mode, ticket.ConfigId, ticket.TeamSize, ticket.EnqueuedMs, ticket.ZoneId, ticket.RatingCenti, ticket.TeamId, ticket.NetworkProfile, ticket.ExpiresMs, ticket.PreferredRegion, ticket.OwnerAccount, ticket.OwnerRevision, ticket.PlayerId); err != nil {
			return nil, err
		}
		if err = deleteTicketRegions(ctx, tx, ticket.PlayerId); err != nil {
			return nil, err
		}
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	if err != nil {
		return nil, err
	}
	exitRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.MatchDirectIntentRecord{SpectatorExits: exits})
	if err != nil {
		return nil, err
	}
	out := proto.Clone(key).(*pb.MatchDirectIntentRecord)
	out.Payload, out.OwnerAccount, out.OwnerRevision, out.SpectatorExits, out.ExitPrepared, out.StartOriginal = payload, actor.OwnerAccount, actor.OwnerRevision, exits, true, raw
	if current == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO match_direct_intent("+directColumns+") VALUES(?,?,?,?,?,0,0,?,?,?,1,?)", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow, payload, actor.OwnerAccount, actor.OwnerRevision, exitRaw, raw)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE match_direct_intent SET spectator_exits=?,exit_prepared=1,start_original=? WHERE kind=? AND owner_id=? AND request_high=? AND request_low=? AND battle_id=0 AND cancelled=0", exitRaw, raw, key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow)
	}
	return out, err
}

func (s *Store) CompletePreparedDirect(ctx context.Context, observed *pb.MatchDirectIntentRecord) (*pb.MatchFormationRecord, error) {
	original, err := DecodeDirectStart(observed)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, original.Tickets); err != nil {
		return nil, err
	}
	players := make([]uint64, 0, len(original.Tickets))
	for _, ticket := range original.Tickets {
		players = append(players, ticket.PlayerId)
	}
	current, err := readDirectTx(ctx, tx, observed)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.ExitPrepared || current.OwnerAccount != observed.OwnerAccount || current.OwnerRevision != observed.OwnerRevision || !bytes.Equal(current.Payload, observed.Payload) || !bytes.Equal(current.StartOriginal, observed.StartOriginal) || !proto.Equal(&pb.MatchDirectIntentRecord{SpectatorExits: current.SpectatorExits}, &pb.MatchDirectIntentRecord{SpectatorExits: observed.SpectatorExits}) {
		return nil, ErrDirectConflict
	}
	if current.Cancelled {
		return nil, ErrDirectCancelled
	}
	if current.BattleId != 0 {
		f, err := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=?", current.BattleId))
		if err != nil {
			return nil, err
		}
		return f, tx.Commit()
	}
	if err = s.RequireNoSpectators(ctx, tx, players); err != nil {
		return nil, err
	}
	if err = s.RequireSpectatorExitsClosedTx(ctx, tx, current.SpectatorExits); err != nil {
		return nil, err
	}
	ordered := append([]*pb.MatchAuthorityTicketRecord(nil), original.Tickets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	expired := false
	for _, want := range ordered {
		actual, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", want.PlayerId))
		if err != nil {
			return nil, err
		}
		if !proto.Equal(actual, want) {
			return nil, ErrDirectConflict
		}
		expired = expired || actual.ExpiresMs <= stamp
		profile, decodeErr := DecodeProfile(actual.NetworkProfile)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if _, ok := Latencies(profile, stamp, s.Config.HardLatencyMs)[original.Formation.PhysicalRegion]; !ok {
			expired = true
		}
	}
	if expired {
		if err = closeDirectPendingTx(ctx, tx, current, original.Tickets); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrDirectCancelled
	}
	for _, ticket := range original.Tickets {
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=1 WHERE player_id=? AND ticket=? AND state=0", ticket.PlayerId, ticket.Ticket); err != nil {
			return nil, err
		}
	}
	f := proto.Clone(original.Formation).(*pb.MatchFormationRecord)
	if err = s.claimTx(ctx, tx, f, original.Tickets, &matchEntryOriginal{direct: current, payload: current.Payload, formation: f, battleID: f.BattleId}); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_direct_intent SET battle_id=? WHERE kind=? AND owner_id=? AND request_high=? AND request_low=? AND battle_id=0 AND cancelled=0", f.BattleId, current.Kind, current.OwnerId, current.RequestHigh, current.RequestLow); err != nil {
		return nil, err
	}
	if current.Kind == 3 {
		if _, err = tx.ExecContext(ctx, "UPDATE match_challenge_invitation SET state=2,battle_id=? WHERE challenge_id=? AND state=4", f.BattleId, current.RequestLow); err != nil {
			return nil, err
		}
	}
	return f, tx.Commit()
}
func closeDirectPendingTx(ctx context.Context, tx *sql.Tx, record *pb.MatchDirectIntentRecord, tickets []*pb.MatchAuthorityTicketRecord) error {
	ordered := append([]*pb.MatchAuthorityTicketRecord(nil), tickets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	for _, ticket := range ordered {
		if _, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND request_id=? AND state=0 AND formation_id=0", ticket.PlayerId, ticket.Ticket, ticket.RequestId); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE match_direct_intent SET cancelled=1 WHERE kind=? AND owner_id=? AND request_high=? AND request_low=? AND battle_id=0", record.Kind, record.OwnerId, record.RequestHigh, record.RequestLow); err != nil {
		return err
	}
	if record.Kind == 3 {
		_, err := tx.ExecContext(ctx, "UPDATE match_challenge_invitation SET state=5 WHERE challenge_id=? AND state=4 AND battle_id=0", record.RequestLow)
		return err
	}
	return nil
}

type DirectCursor struct {
	Kind             uint32
	Owner, High, Low uint64
}

func (s *Store) DirectAdmissionHighWater(ctx context.Context) (DirectCursor, error) {
	var cursor DirectCursor
	err := s.DB.QueryRowContext(ctx, "SELECT kind,owner_id,request_high,request_low FROM match_direct_intent WHERE exit_prepared=1 AND cancelled=0 AND battle_id=0 ORDER BY kind DESC,owner_id DESC,request_high DESC,request_low DESC LIMIT 1").Scan(&cursor.Kind, &cursor.Owner, &cursor.High, &cursor.Low)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectCursor{}, nil
	}
	return cursor, err
}
func (s *Store) DirectAdmissionsThrough(ctx context.Context, after, through DirectCursor) ([]*pb.MatchDirectIntentRecord, error) {
	columns := []string{"kind", "owner_id", "request_high", "request_low"}
	values := func(c DirectCursor) []any { return []any{c.Kind, c.Owner, c.High, c.Low} }
	// 元组keyset匹配同一复合索引；固定尾界不被持续新请求延长。
	keys := "(" + strings.Join(columns, ",") + ")"
	args := append(values(after), values(through)...)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+directColumns+" FROM match_direct_intent WHERE exit_prepared=1 AND cancelled=0 AND battle_id=0 AND "+keys+">(?,?,?,?) AND "+keys+"<=(?,?,?,?) ORDER BY kind,owner_id,request_high,request_low LIMIT 8", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchDirectIntentRecord
	for rows.Next() {
		record, err := scanDirect(rows)
		if err != nil {
			return nil, err
		}
		if _, err = DecodeDirectStart(record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
