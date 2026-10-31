package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

// PrepareJoin先持久原请求、原票和一次性观战退出原件，再允许发送Remove。
// 非空state0票是待提交占用；Cancel或成功Join才能释放，TTL不冒充远端取消证明。
func (s *Store) PrepareJoin(ctx context.Context, offered *pb.MatchAuthorityTicketRecord, request *pb.JoinQueueRequest, binding string) (*pb.MatchAuthorityTicketRecord, *pb.MatchSpectatorRecord, bool, error) {
	if offered == nil || request == nil || offered.RequestId != request.RequestId || offered.PlayerId == 0 || offered.Ticket == "" {
		return nil, nil, false, ErrOriginalPlayer
	}
	high, low, err := joinRequestParts(offered.RequestId)
	if err != nil {
		return nil, nil, false, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, nil, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, offered.PlayerId); err != nil {
		return nil, nil, false, err
	}
	actor := originalPlayer(ctx, offered.PlayerId)
	intent := &pb.MatchJoinIntentRecord{}
	err = tx.QueryRowContext(ctx, "SELECT ticket_record,cancelled,owner_account,owner_revision,spectator_exit,exit_prepared,request_payload FROM match_join_intent WHERE player_id=? AND request_high=? AND request_low=? FOR UPDATE", offered.PlayerId, high, low).Scan(&intent.TicketRecord, &intent.Cancelled, &intent.OwnerAccount, &intent.OwnerRevision, &intent.SpectatorExit, &intent.ExitPrepared, &intent.RequestPayload)
	if err == nil {
		if intent.OwnerAccount != actor.OwnerAccount || intent.OwnerRevision != actor.OwnerRevision {
			return nil, nil, false, ErrOriginalPlayer
		}
		if intent.Cancelled {
			return nil, nil, false, ErrJoinCancelled
		}
		original := &pb.MatchAuthorityTicketRecord{}
		if proto.Unmarshal(intent.TicketRecord, original) != nil || original.Ticket == "" || original.RequestId != offered.RequestId || original.OwnerAccount != actor.OwnerAccount || original.OwnerRevision != actor.OwnerRevision {
			return nil, nil, false, ErrOriginalPlayer
		}
		if original.Mode != offered.Mode || original.ConfigId != offered.ConfigId || original.TeamSize != offered.TeamSize || (intent.ExitPrepared && !bytes.Equal(raw, intent.RequestPayload)) {
			return nil, nil, false, errors.New("重复请求的匹配参数不同")
		}
		// 已提交的旧意图不捕获或退出后来新开的观战。
		if original.State != 0 {
			return original, nil, false, tx.Commit()
		}
		if !intent.ExitPrepared {
			return nil, nil, false, ErrOriginalPlayer
		}
		var exit *pb.MatchSpectatorRecord
		if len(intent.SpectatorExit) != 0 {
			exit = &pb.MatchSpectatorRecord{}
			if proto.Unmarshal(intent.SpectatorExit, exit) != nil || exit.PlayerId != original.PlayerId {
				return nil, nil, false, ErrOriginalPlayer
			}
		}
		return original, exit, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, err
	}
	if err = s.requireMatchEntryTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}, nil); err != nil {
		return nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", offered.PlayerId, []byte{}); err != nil {
		return nil, nil, false, err
	}
	current, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", offered.PlayerId))
	if err != nil {
		return nil, nil, false, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return nil, nil, false, err
	}
	if TicketOccupied(current) && !(current.State == 1 && current.ExpiresMs <= n) {
		if current.OwnerAccount != actor.OwnerAccount || current.OwnerRevision != actor.OwnerRevision {
			return nil, nil, false, ErrOriginalPlayer
		}
		return current, nil, false, tx.Commit()
	}
	original := proto.Clone(offered).(*pb.MatchAuthorityTicketRecord)
	original.OwnerAccount, original.OwnerRevision = actor.OwnerAccount, actor.OwnerRevision
	if original.NetworkProfile, original.ExpiresMs, err = s.joinProfileTx(ctx, tx, original, request.NetworkPlanId, binding); err != nil {
		return nil, nil, false, err
	}
	exits, err := s.CaptureSpectatorExitsTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{original})
	if err != nil {
		return nil, nil, false, err
	}
	var exit *pb.MatchSpectatorRecord
	exitRaw := []byte{}
	if len(exits) != 0 {
		exit = exits[0]
		if exitRaw, err = (proto.MarshalOptions{Deterministic: true}).Marshal(exit); err != nil {
			return nil, nil, false, err
		}
	}
	original.State = 0
	originalRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	if err != nil {
		return nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_join_intent(player_id,request_id,ticket_record,cancelled,request_high,request_low,owner_account,owner_revision,spectator_exit,exit_prepared,request_payload,state) VALUES(?,?,?,0,?,?,?,?,?,1,?,1)", original.PlayerId, original.RequestId, originalRaw, high, low, original.OwnerAccount, original.OwnerRevision, exitRaw, raw); err != nil {
		return nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket=?,request_id=?,mode=?,config_id=?,team_size=?,state=0,formation_id=0,team_id=0,owner_account=?,owner_revision=? WHERE player_id=?", original.Ticket, original.RequestId, original.Mode, original.ConfigId, original.TeamSize, original.OwnerAccount, original.OwnerRevision, original.PlayerId); err != nil {
		return nil, nil, false, err
	}
	if err = deleteTicketRegions(ctx, tx, original.PlayerId); err != nil {
		return nil, nil, false, err
	}
	return original, exit, true, tx.Commit()
}

func (s *Store) joinProfileTx(ctx context.Context, tx *sql.Tx, ticket *pb.MatchAuthorityTicketRecord, plan, binding string) ([]byte, uint64, error) {
	var raw []byte
	var expires uint64
	if err := tx.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=?", ticket.PlayerId).Scan(&raw, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrProbeRequired
		}
		return nil, 0, err
	}
	profile, err := DecodeProfile(raw)
	if err != nil {
		return nil, 0, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	if profile.PlanId != plan || profile.SessionBinding != binding || expires <= n {
		return nil, 0, ErrProbeRequired
	}
	if len(Latencies(profile, n, s.Config.HardLatencyMs)) == 0 {
		return nil, 0, ErrNoRegionWithinLatency
	}
	if profile.OwnerAccount != ticket.OwnerAccount || profile.OwnerRevision != ticket.OwnerRevision {
		return nil, 0, ErrOriginalPlayer
	}
	return raw, expires, nil
}
