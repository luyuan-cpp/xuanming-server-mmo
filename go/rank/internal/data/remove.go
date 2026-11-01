package data

import (
	"context"
	"database/sql"

	rankpb "proto/rank"
	"rank/internal/metrics"
	"shared/serviceauth"
)

func (r *Repo) Remove(ctx context.Context, caller string, in *rankpb.RemoveRankEntryRequest, hide bool) (err error) {
	operation := metrics.OperationRemove
	if hide {
		operation = metrics.OperationDelete
	}
	replay := false
	defer func() { observeWrite(operation, err, replay) }()
	if in == nil || in.BoardId == 0 || in.Generation == 0 || in.RequestId == 0 || (hide && in.EntityId != 0) || (!hide && in.EntityId == 0) {
		return ErrInvalid
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return err
	}
	// 同一request_id不能把删除成员与隐藏整代混为同一操作。
	if hide {
		digest = "hide:" + digest
	} else {
		digest = "remove:" + digest
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		b, err := scanBoard(tx.QueryRowContext(ctx, "SELECT "+boardColumns+" FROM rank_board WHERE board_id=? FOR UPDATE", in.BoardId))
		if err != nil {
			return err
		}
		if b.Producer != caller {
			return ErrDenied
		}
		if b.Generation != in.Generation {
			return ErrConflict
		}
		var existing string
		err = tx.QueryRowContext(ctx, "SELECT request_hash FROM rank_mutation WHERE board_id=? AND generation=? AND request_id=?", in.BoardId, in.Generation, in.RequestId).Scan(&existing)
		if err == nil {
			if existing != digest {
				return ErrConflict
			}
			replay = true
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if !hide && (b.State != uint32(rankpb.RankGenerationState_RANK_GENERATION_OPEN) || b.DeletedMs != 0) {
			return ErrClosed
		}
		if hide {
			_, err = tx.ExecContext(ctx, "UPDATE rank_board SET deleted_ms=? WHERE board_id=?", uint64(r.now().UnixMilli()), b.BoardId)
		} else {
			// 墓碑和删行同一事务:批次的到达顺序不代表发生顺序,没有墓碑时迟到的旧批次会把人带回榜上。
			if _, err = tx.ExecContext(ctx, "INSERT INTO rank_removed_entry (board_id,generation,entity_id,removed_ms) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE removed_ms=removed_ms", b.BoardId, b.Generation, in.EntityId, uint64(r.now().UnixMilli())); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "DELETE FROM rank_score WHERE board_id=? AND generation=? AND entity_id=?", b.BoardId, b.Generation, in.EntityId)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO rank_mutation (board_id,generation,request_id,request_hash,created_ms) VALUES (?,?,?,?,?)", b.BoardId, b.Generation, in.RequestId, digest, uint64(r.now().UnixMilli()))
		return err
	})
}

// removedEntries 读这一代被移除的实体。移除是运营操作,一代只有少数几条,整代读一次比逐条查便宜。
func removedEntries(ctx context.Context, tx *sql.Tx, board, generation uint64) (map[uint64]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT entity_id FROM rank_removed_entry WHERE board_id=? AND generation=?", board, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	removed := make(map[uint64]bool)
	for rows.Next() {
		var entity uint64
		if err := rows.Scan(&entity); err != nil {
			return nil, err
		}
		removed[entity] = true
	}
	return removed, rows.Err()
}
