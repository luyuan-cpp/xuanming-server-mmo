package data

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	rankpb "proto/rank"
	"rank/internal/metrics"
	"shared/mailgrant"
	"shared/serviceauth"
)

var (
	ErrNotFound = errors.New("排行榜不存在")
	ErrClosed   = errors.New("排行榜本代已关闭")
	ErrConflict = errors.New("排行榜请求冲突")
	ErrCursor   = errors.New("排行榜输入游标未齐")
	ErrInvalid  = errors.New("排行榜参数无效")
	ErrDenied   = errors.New("排行榜生产者无权操作")
)

type NextID func(context.Context) (uint64, error)
type Repo struct {
	db                                 *sql.DB
	boardIDs, settlementIDs, rewardIDs NextID
	now                                func() time.Time
	itemExists                         func(uint32) bool
}

func New(db *sql.DB, boardIDs, settlementIDs, rewardIDs NextID, itemExists func(uint32) bool) *Repo {
	return &Repo{db: db, boardIDs: boardIDs, settlementIDs: settlementIDs, rewardIDs: rewardIDs, now: time.Now, itemExists: itemExists}
}

const boardColumns = "board_id,board_type,scope,scope_id,period,generation,producer,source_epoch,committed_cursor,ascending,state,settlement_id,open_request_id,open_request_hash,created_ms,sealed_ms,deleted_ms"

type scanner interface{ Scan(...any) error }

func scanBoard(row scanner) (*rankpb.RankBoardRecord, error) {
	b := &rankpb.RankBoardRecord{}
	err := row.Scan(&b.BoardId, &b.BoardType, &b.Scope, &b.ScopeId, &b.Period, &b.Generation, &b.Producer, &b.SourceEpoch, &b.CommittedCursor, &b.Ascending, &b.State, &b.SettlementId, &b.OpenRequestId, &b.OpenRequestHash, &b.CreatedMs, &b.SealedMs, &b.DeletedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (r *Repo) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit()
			return err
		} // 提交结果未知不自动重做业务。
		_ = tx.Rollback()
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || (mysqlErr.Number != 1213 && mysqlErr.Number != 1205) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ErrConflict
}

func validBoard(b *rankpb.RankBoardKey) bool {
	return b != nil && b.BoardType > 0 && b.Scope >= rankpb.RankScope_RANK_SCOPE_GLOBAL && b.Scope <= rankpb.RankScope_RANK_SCOPE_CUSTOM && len(b.Period) > 0 && len(b.Period) <= 64 && b.Generation > 0
}

func (r *Repo) Open(ctx context.Context, caller string, in *rankpb.OpenRankGenerationRequest) (result *rankpb.OpenRankGenerationResponse, err error) {
	defer func() { observeWrite(metrics.OperationOpen, err, result.GetDuplicated()) }()
	if in == nil || !validBoard(in.Board) || caller != in.Producer || in.SourceEpoch == 0 || in.RequestId == 0 {
		return nil, ErrInvalid
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	id, err := r.boardIDs(ctx)
	if err != nil {
		return nil, err
	}
	now := uint64(r.now().UnixMilli())
	b := in.Board
	_, err = r.db.ExecContext(ctx, "INSERT INTO rank_board ("+boardColumns+") VALUES (?,?,?,?,?,?,?,?,0,?,1,0,?,?,?,0,0)", id, b.BoardType, uint32(b.Scope), b.ScopeId, b.Period, b.Generation, caller, in.SourceEpoch, boolInt(in.Ascending), in.RequestId, digest, now)
	if err == nil {
		return &rankpb.OpenRankGenerationResponse{BoardId: id}, nil
	}
	var duplicate *mysql.MySQLError
	if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
		return nil, err
	}
	existing, err := scanBoard(r.db.QueryRowContext(ctx, "SELECT "+boardColumns+" FROM rank_board WHERE board_type=? AND scope=? AND scope_id=? AND period=? AND generation=?", b.BoardType, uint32(b.Scope), b.ScopeId, b.Period, b.Generation))
	if err != nil {
		return nil, err
	}
	if existing.OpenRequestHash != digest || existing.DeletedMs != 0 {
		return nil, ErrConflict
	}
	return &rankpb.OpenRankGenerationResponse{BoardId: existing.BoardId, Duplicated: true}, nil
}
func boolInt(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func (r *Repo) Submit(ctx context.Context, caller string, in *rankpb.SubmitRankScoresRequest) (result *rankpb.SubmitRankScoresResponse, err error) {
	defer func() { observeWrite(metrics.OperationSubmit, err, result.GetDuplicated()) }()
	if in == nil || in.BoardId == 0 || in.Generation == 0 || in.SourceEpoch == 0 || in.SourceCursor == 0 || in.RequestId == 0 || len(in.Entries) == 0 || len(in.Entries) > 128 || in.Mode < rankpb.RankSubmitMode_RANK_SUBMIT_SET_IF_HIGHER || in.Mode > rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT {
		return nil, ErrInvalid
	}
	entries := append([]*rankpb.RankScoreInput(nil), in.Entries...)
	for _, entry := range entries {
		if entry == nil || entry.EntityId == 0 || entry.AchievedMs == 0 {
			return nil, ErrInvalid
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntityId < entries[j].EntityId })
	for index := 1; index < len(entries); index++ {
		if entries[index-1].EntityId == entries[index].EntityId {
			return nil, ErrInvalid
		}
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	result = &rankpb.SubmitRankScoresResponse{}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		b, err := scanBoard(tx.QueryRowContext(ctx, "SELECT "+boardColumns+" FROM rank_board WHERE board_id=? FOR UPDATE", in.BoardId))
		if err != nil {
			return err
		}
		if b.Producer != caller {
			return ErrDenied
		}
		if b.Generation != in.Generation || b.SourceEpoch != in.SourceEpoch || b.DeletedMs != 0 {
			return ErrConflict
		}
		// 游标按片各自连续:幂等键是 (shard, cursor)。上面的 board 行锁把同榜的 Submit 串行了,
		// 这里读到的 MAX 就是提交点的值;收据保留到本代结束,MAX 不会因清理回退。
		var committed uint64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(source_cursor),0) FROM rank_input_receipt WHERE board_id=? AND generation=? AND source_shard=?", in.BoardId, in.Generation, in.SourceShard).Scan(&committed); err != nil {
			return err
		}
		if in.SourceCursor <= committed {
			var existing string
			if err := tx.QueryRowContext(ctx, "SELECT request_hash FROM rank_input_receipt WHERE board_id=? AND generation=? AND source_shard=? AND source_cursor=?", in.BoardId, in.Generation, in.SourceShard, in.SourceCursor).Scan(&existing); err != nil {
				return err
			}
			if existing != digest {
				return ErrConflict
			}
			result.CommittedCursor = committed
			result.Duplicated = true
			return nil
		}
		if b.State != uint32(rankpb.RankGenerationState_RANK_GENERATION_OPEN) {
			return ErrClosed
		}
		if committed == math.MaxUint64 || in.SourceCursor != committed+1 {
			return ErrCursor
		}
		for _, entry := range entries {
			var old int64
			var achieved, version uint64
			err := tx.QueryRowContext(ctx, "SELECT score,achieved_ms,score_version FROM rank_score WHERE board_id=? AND generation=? AND entity_id=?", in.BoardId, in.Generation, entry.EntityId).Scan(&old, &achieved, &version)
			found := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// 不同片的批次到达顺序不代表发生顺序:带版本时旧版本迟到直接跳过,否则榜分会退回旧值。
			if found && entry.ScoreVersion != 0 && entry.ScoreVersion <= version {
				continue
			}
			next := entry.Score
			if in.Mode == rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT {
				if (next > 0 && old > math.MaxInt64-next) || (next < 0 && old < math.MinInt64-next) {
					return ErrInvalid
				}
				next = old + next
			}
			if in.Mode == rankpb.RankSubmitMode_RANK_SUBMIT_SET_IF_HIGHER && found && ((b.Ascending == 0 && next <= old) || (b.Ascending != 0 && next >= old)) {
				continue
			}
			// 带版本的生产者自己定 achieved_ms(分数最后一次变化的时刻),和到达顺序无关;不带版本的按「分数变了才更新」。
			if !found || next != old || entry.ScoreVersion != 0 {
				achieved = entry.AchievedMs
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO rank_score (board_id,generation,entity_id,score,achieved_ms,source_cursor,score_version) VALUES (?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE score=VALUES(score),achieved_ms=VALUES(achieved_ms),source_cursor=VALUES(source_cursor),score_version=VALUES(score_version)", in.BoardId, in.Generation, entry.EntityId, next, achieved, in.SourceCursor, entry.ScoreVersion); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO rank_input_receipt (board_id,generation,source_shard,source_cursor,request_id,request_hash,committed_ms) VALUES (?,?,?,?,?,?,?)", in.BoardId, in.Generation, in.SourceShard, in.SourceCursor, in.RequestId, digest, uint64(r.now().UnixMilli())); err != nil {
			return err
		}
		// 榜上的 committed_cursor 是各片批次总数,封榜时与生产者的各片最终游标之和比对。
		if _, err := tx.ExecContext(ctx, "UPDATE rank_board SET committed_cursor=committed_cursor+1 WHERE board_id=?", in.BoardId); err != nil {
			return err
		}
		result.CommittedCursor = in.SourceCursor
		return nil
	})
	return result, err
}

// Freeze在一笔事务内留下完整快照与全部邮件意图，不依赖后续补扫生成缺失奖励。
func (r *Repo) Freeze(ctx context.Context, caller string, in *rankpb.FreezeRankGenerationRequest) (result *rankpb.FreezeRankGenerationResponse, err error) {
	defer func() { observeWrite(metrics.OperationFreeze, err, result.GetDuplicated()) }()
	if in == nil || in.BoardId == 0 || in.Generation == 0 || in.SourceEpoch == 0 || in.RequestId == 0 || len(in.Rewards) > 100 {
		return nil, ErrInvalid
	}
	top := in.TopN
	if top == 0 {
		top = 100
	}
	if top > 1000 {
		return nil, ErrInvalid
	}
	for _, tier := range in.Rewards {
		if tier == nil || tier.FirstRank == 0 || tier.LastRank < tier.FirstRank || tier.LastRank > 1000 || tier.Attachments == nil {
			return nil, ErrInvalid
		}
		if tier.LastRank > top {
			return nil, ErrInvalid
		}
		bundle, e := mailgrant.Bundle(tier.Attachments, r.itemExists)
		if e != nil || (len(bundle.GetCurrencies()) == 0 && len(bundle.GetItems()) == 0) {
			return nil, ErrInvalid
		}
	}
	// 重叠档位会让同一名次出现两份奖；在任何写操作之前拒绝。
	for i, a := range in.Rewards {
		for j, b := range in.Rewards {
			if i < j && a.FirstRank <= b.LastRank && b.FirstRank <= a.LastRank {
				return nil, ErrInvalid
			}
		}
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	settlement, err := r.settlementIDs(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, top)
	for i := range ids {
		ids[i], err = r.rewardIDs(ctx)
		if err != nil {
			return nil, err
		}
	}
	result = &rankpb.FreezeRankGenerationResponse{}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		b, err := scanBoard(tx.QueryRowContext(ctx, "SELECT "+boardColumns+" FROM rank_board WHERE board_id=? FOR UPDATE", in.BoardId))
		if err != nil {
			return err
		}
		if b.Producer != caller {
			return ErrDenied
		}
		if b.Generation != in.Generation || b.SourceEpoch != in.SourceEpoch || b.DeletedMs != 0 {
			return ErrConflict
		}
		if b.SettlementId != 0 {
			var hash string
			if err := tx.QueryRowContext(ctx, "SELECT request_hash,winner_count FROM rank_settlement WHERE settlement_id=?", b.SettlementId).Scan(&hash, &result.WinnerCount); err != nil {
				return err
			}
			if hash != digest {
				return ErrConflict
			}
			result.SettlementId = b.SettlementId
			result.Duplicated = true
			return nil
		}
		if b.State != uint32(rankpb.RankGenerationState_RANK_GENERATION_OPEN) {
			return ErrClosed
		}
		if b.CommittedCursor != in.ExpectedSourceCursor {
			return ErrCursor
		}
		order := "score DESC,achieved_ms ASC,entity_id ASC"
		if b.Ascending != 0 {
			order = "score ASC,achieved_ms ASC,entity_id ASC"
		}
		rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? ORDER BY "+order+" LIMIT ?", b.BoardId, b.Generation, top)
		if err != nil {
			return err
		}
		var winners []*rankpb.RankSnapshotRecord
		for rows.Next() {
			row := &rankpb.RankSnapshotRecord{SettlementId: settlement, Rank: uint64(len(winners) + 1)}
			if err := rows.Scan(&row.EntityId, &row.Score, &row.AchievedMs); err != nil {
				rows.Close()
				return err
			}
			winners = append(winners, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := uint64(r.now().UnixMilli())
		if _, err := tx.ExecContext(ctx, "INSERT INTO rank_settlement (settlement_id,board_id,generation,source_epoch,final_cursor,request_id,request_hash,winner_count,sealed_ms) VALUES (?,?,?,?,?,?,?,?,?)", settlement, b.BoardId, b.Generation, b.SourceEpoch, b.CommittedCursor, in.RequestId, digest, len(winners), now); err != nil {
			return err
		}
		for index, winner := range winners {
			if _, err := tx.ExecContext(ctx, "INSERT INTO rank_snapshot (settlement_id,`rank`,entity_id,score,achieved_ms) VALUES (?,?,?,?,?)", settlement, winner.Rank, winner.EntityId, winner.Score, winner.AchievedMs); err != nil {
				return err
			}
			for _, tier := range in.Rewards {
				if winner.Rank >= uint64(tier.FirstRank) && winner.Rank <= uint64(tier.LastRank) {
					payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(tier.Attachments)
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, "INSERT INTO rank_mail_reward (reward_id,settlement_id,player_id,`rank`,attachments,status,mail_id,next_attempt_ms,lease_until_ms,lease_token,attempts,created_ms,updated_ms) VALUES (?,?,?,?,?,1,0,?,0,0,0,?,?)", ids[index], settlement, winner.EntityId, winner.Rank, payload, now, now, now); err != nil {
						return err
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE rank_board SET state=?,settlement_id=?,sealed_ms=? WHERE board_id=?", uint32(rankpb.RankGenerationState_RANK_GENERATION_SEALED), settlement, now, b.BoardId); err != nil {
			return err
		}
		result.SettlementId = settlement
		result.WinnerCount = uint64(len(winners))
		return nil
	})
	return result, err
}
