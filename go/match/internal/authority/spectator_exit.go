package authority

import (
	"bytes"
	"context"
	"database/sql"
	"sort"

	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

func sameSpectatorOriginal(a, b *pb.MatchSpectatorRecord) bool {
	if a == nil || b == nil {
		return false
	}
	a = proto.Clone(a).(*pb.MatchSpectatorRecord)
	b = proto.Clone(b).(*pb.MatchSpectatorRecord)
	a.State, a.UpdatedMs, b.State, b.UpdatedMs = 0, 0, 0, 0
	return proto.Equal(a, b)
}
func writeSpectatorStateTx(ctx context.Context, tx *sql.Tx, current *pb.MatchSpectatorRecord, state uint32) error {
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	current.State, current.UpdatedMs = state, stamp
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(current)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_spectator SET state=?,updated_ms=? WHERE player_id=? AND operation_id=?", state, stamp, current.PlayerId, current.OperationId); err != nil {
		return err
	}
	intentState := uint32(2)
	if state == 5 {
		intentState = 5
	}
	result, err := tx.ExecContext(ctx, "UPDATE match_spectator_intent SET original=?,state=? WHERE player_id=? AND request_high=? AND request_low=?", raw, intentState, current.PlayerId, current.RequestHigh, current.RequestLow)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrOriginalPlayer
	}
	return nil
}

// 调用方已持全部原P；必须把返回的完整原件、捕获标志、业务原意图/名册授权同tx提交。
// 捕获只发生一次，不按重试时的当前watch补原件；事务外才允许发送原Remove。
func (s *Store) CaptureSpectatorExitsTx(ctx context.Context, tx *sql.Tx, originals []*pb.MatchAuthorityTicketRecord) ([]*pb.MatchSpectatorRecord, error) {
	if len(originals) == 0 {
		return nil, ErrOriginalPlayer
	}
	originals = append([]*pb.MatchAuthorityTicketRecord(nil), originals...)
	for _, actor := range originals {
		if actor == nil || actor.PlayerId == 0 {
			return nil, ErrOriginalPlayer
		}
	}
	sort.Slice(originals, func(i, j int) bool { return originals[i].PlayerId < originals[j].PlayerId })
	var exits []*pb.MatchSpectatorRecord
	for index, actor := range originals {
		if actor == nil || actor.PlayerId == 0 {
			return nil, ErrOriginalPlayer
		}
		if index > 0 && originals[index-1].PlayerId == actor.PlayerId {
			if originals[index-1].OwnerAccount != actor.OwnerAccount || originals[index-1].OwnerRevision != actor.OwnerRevision {
				return nil, ErrOriginalPlayer
			}
			continue
		}
		head, err := readSpectatorHead(ctx, tx, actor.PlayerId)
		if err != nil {
			return nil, err
		}
		if head != "" {
			high, low, _ := joinRequestParts(head)
			intent, err := readSpectatorIntent(ctx, tx, actor.PlayerId, high, low)
			if err != nil {
				return nil, err
			}
			if intent == nil {
				return nil, ErrOriginalPlayer
			}
			// 新参战意图也终结先前尚未prepare的Watch，防其在本次匹配结束后才复活。
			if intent.State == 1 {
				if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET state=5 WHERE player_id=? AND request_high=? AND request_low=? AND state=1", actor.PlayerId, high, low); err != nil {
					return nil, err
				}
			}
		}
		current, err := readSpectator(ctx, tx, actor.PlayerId)
		if err != nil {
			return nil, err
		}
		if current == nil || current.State == 5 {
			continue
		}
		if head == "" {
			return nil, ErrOriginalPlayer
		}
		if !sameSpectatorOwner(current, actor) {
			return nil, ErrOriginalPlayer
		}
		receipt, err := readSpectatorIntent(ctx, tx, current.PlayerId, current.RequestHigh, current.RequestLow)
		if err != nil {
			return nil, err
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(current)
		if err != nil {
			return nil, err
		}
		if receipt == nil || receipt.State != 2 || !bytes.Equal(receipt.Original, raw) {
			return nil, ErrOriginalPlayer
		}
		if current.State != 3 {
			if err = writeSpectatorStateTx(ctx, tx, current, 3); err != nil {
				return nil, err
			}
		}
		exits = append(exits, proto.Clone(current).(*pb.MatchSpectatorRecord))
	}
	return exits, nil
}

func spectatorExitClosedTx(ctx context.Context, tx *sql.Tx, original *pb.MatchSpectatorRecord) (bool, error) {
	if validateSpectator(original) != nil {
		return false, ErrOriginalPlayer
	}
	receipt, err := readSpectatorIntent(ctx, tx, original.PlayerId, original.RequestHigh, original.RequestLow)
	if err != nil {
		return false, err
	}
	if receipt == nil || len(receipt.Original) == 0 {
		return false, ErrOriginalPlayer
	}
	actual := &pb.MatchSpectatorRecord{}
	if proto.Unmarshal(receipt.Original, actual) != nil || !sameSpectatorOriginal(actual, original) {
		return false, ErrOriginalPlayer
	}
	return receipt.State == 5 && actual.State == 5, nil
}
func (s *Store) SpectatorExitClosed(ctx context.Context, original *pb.MatchSpectatorRecord) (closed bool, err error) {
	err = s.Fenced(ctx, func() (e error) { closed, e = s.spectatorExitClosedOnce(ctx, original); return e })
	return closed, err
}

func (s *Store) spectatorExitClosedOnce(ctx context.Context, original *pb.MatchSpectatorRecord) (bool, error) {
	if validateSpectator(original) != nil {
		return false, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{spectatorActor(original)}); err != nil {
		return false, err
	}
	closed, err := spectatorExitClosedTx(ctx, tx, original)
	if err != nil {
		return false, err
	}
	return closed, tx.Commit()
}

// 调用方持完整P；只沿保存的原件验证正证明，另由RequireNoSpectators拒绝后来新占用。
func (s *Store) RequireSpectatorExitsClosedTx(ctx context.Context, tx *sql.Tx, exits []*pb.MatchSpectatorRecord) error {
	for _, original := range exits {
		closed, err := spectatorExitClosedTx(ctx, tx, original)
		if err != nil {
			return err
		}
		if !closed {
			return ErrSpectatorBusy
		}
	}
	return nil
}
