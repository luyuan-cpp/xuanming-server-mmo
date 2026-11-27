package data

import (
	"context"
	"database/sql"
	"errors"

	rankpb "proto/rank"
	"rank/internal/metrics"
	"shared/serviceauth"
)

// AdminCaller 是后台(java/gm_admin)在 rank 的调用方名(rank.yaml 的 Producers.admin)。它的删除都经双人复核批准
// (internal/server.admitApproval),所以可以删任意榜上的单个条目(反作弊处置)。
const AdminCaller = "admin"

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
		// 开榜方可以删自己榜上的条目、隐藏自己的整代;后台经双人复核可以删任意榜上的单个条目。整代隐藏仍只许开榜方:
		// 系统开的榜(match 的段位榜、赛季榜)被整代隐藏之后,开榜方的提交会一直失败,这一步要开榜方自己做。
		// shortcut: 判重键 (board, generation, request_id) 不含调用方,后台的批准号与 match 摘角色用的玩家号理论上可能撞号
		// (两边都是 63 位打散值,概率可以忽略);真要杜绝就把调用方并进 rank_mutation 的主键。
		if b.Producer != caller && (hide || caller != AdminCaller) {
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
		// 整代操作:持榜行锁再按片号升序锁全部片行,与在途的 Submit 互斥(shards.go 的锁序)。Submit 只锁自己那一片,
		// 不锁全部片行的话,在途批次读到的还是移除 / 隐藏之前的状态,会把人写回榜上或写进已隐藏的一代。
		shards, err := lockAllShards(ctx, tx, b.BoardId, b.Generation)
		if err != nil {
			return err
		}
		if hide {
			_, err = tx.ExecContext(ctx, "UPDATE rank_board SET deleted_ms=? WHERE board_id=?", uint64(r.now().UnixMilli()), b.BoardId)
		} else {
			// 墓碑和删行同一事务:批次的到达顺序不代表发生顺序,没有墓碑时迟到的旧批次会把人带回榜上。
			if _, err = tx.ExecContext(ctx, "INSERT INTO rank_removed_entry (board_id,generation,entity_id,removed_ms) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE removed_ms=removed_ms", b.BoardId, b.Generation, in.EntityId, uint64(r.now().UnixMilli())); err != nil {
				return err
			}
			err = deleteScoreAndCount(ctx, tx, b, shards, in.EntityId)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO rank_mutation (board_id,generation,request_id,request_hash,created_ms) VALUES (?,?,?,?,?)", b.BoardId, b.Generation, in.RequestId, digest, uint64(r.now().UnixMilli()))
		return err
	})
}

// errEntriesDrift:删掉了一行,本代各片行的计数却都已是 0。计数与 rank_score 对不上只可能是数据被绕过本服务改过,
// 回错误让整笔事务回滚(fail-closed),不把计数夹到 0 了事。
var errEntriesDrift = errors.New("排行榜条目计数与数据不符")

// deleteScoreAndCount 删掉 entity 在本代的分数行;真的删到一行时,从片号最小、计数大于 0 的片行上减一
// (shardToDecrement:只有各片之和有意义)。调用方持榜行锁与全部片行锁,shards 是锁住时读到的片行。
func deleteScoreAndCount(ctx context.Context, tx *sql.Tx, b *rankpb.RankBoardRecord, shards []shardRow, entity uint64) error {
	result, err := tx.ExecContext(ctx, "DELETE FROM rank_score WHERE board_id=? AND generation=? AND entity_id=?", b.BoardId, b.Generation, entity)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return nil
	}
	shard, ok := shardToDecrement(shards)
	if !ok {
		return errEntriesDrift
	}
	result, err = tx.ExecContext(ctx, "UPDATE rank_board_shard SET entries=entries-1 WHERE board_id=? AND generation=? AND source_shard=? AND entries>0", b.BoardId, b.Generation, shard)
	if err != nil {
		return err
	}
	if updated, err := result.RowsAffected(); err != nil {
		return err
	} else if updated != 1 {
		return errEntriesDrift
	}
	return nil
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
