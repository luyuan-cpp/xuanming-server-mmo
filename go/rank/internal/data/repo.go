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

	// rewardMailWindow 是封榜到奖励邮件过期的时长,封榜时写进 rank_mail_reward.expire_ms(见 RewardMailWindow)。
	rewardMailWindow time.Duration
}

func New(db *sql.DB, boardIDs, settlementIDs, rewardIDs NextID, itemExists func(uint32) bool) *Repo {
	return &Repo{db: db, boardIDs: boardIDs, settlementIDs: settlementIDs, rewardIDs: rewardIDs, now: time.Now, itemExists: itemExists, rewardMailWindow: MaxRewardMailWindow}
}

// 本代已落账的批次总数与现有实体数不在榜行上,是各片行之和(rank_board_shard,见 shards.go)。
const boardColumns = "board_id,board_type,scope,scope_id,period,generation,producer,source_epoch,ascending,state,settlement_id,open_request_id,open_request_hash,created_ms,sealed_ms,deleted_ms"

type scanner interface{ Scan(...any) error }

func scanBoard(row scanner) (*rankpb.RankBoardRecord, error) {
	b := &rankpb.RankBoardRecord{}
	err := row.Scan(&b.BoardId, &b.BoardType, &b.Scope, &b.ScopeId, &b.Period, &b.Generation, &b.Producer, &b.SourceEpoch, &b.Ascending, &b.State, &b.SettlementId, &b.OpenRequestId, &b.OpenRequestHash, &b.CreatedMs, &b.SealedMs, &b.DeletedMs)
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
		if !retryableTxError(err) || attempt == 2 {
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

// retryableTxError:整笔回滚之后原样重做是安全的失败。死锁(1213)与锁等待超时(1205)由库判;errScoreInsertRace 是
// 别的片的批次抢先插入了同一个实体,重做时它已是已有行,会被锁住再合并。
func retryableTxError(err error) bool {
	if errors.Is(err, errScoreInsertRace) {
		return true
	}
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205)
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
	_, err = r.db.ExecContext(ctx, "INSERT INTO rank_board ("+boardColumns+") VALUES (?,?,?,?,?,?,?,?,?,1,0,?,?,?,0,0)", id, b.BoardType, uint32(b.Scope), b.ScopeId, b.Period, b.Generation, caller, in.SourceEpoch, boolInt(in.Ascending), in.RequestId, digest, now)
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
	if in == nil || in.BoardId == 0 || in.Generation == 0 || in.SourceEpoch == 0 || in.SourceCursor == 0 || in.RequestId == 0 || in.SourceShard >= maxSourceShards || len(in.Entries) == 0 || len(in.Entries) > 128 || in.Mode < rankpb.RankSubmitMode_RANK_SUBMIT_SET_IF_HIGHER || in.Mode > rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT {
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
		// 先锁本片的片行:同片的批次由它串行,不同片的批次互不等待(2026-11-11,缺口清单第 17 条;锁序见 shards.go)。
		shard, err := lockShard(ctx, tx, in.BoardId, in.Generation, in.SourceShard)
		if err != nil {
			return err
		}
		// 本片还没有片行:要在榜行锁里建,和封榜等整代操作互斥。已有片行时榜行只做普通读:事务是读已提交,
		// 持片行锁之后读到的就是最新提交的榜行;封榜、移除要先拿到全部片行锁才能提交,在途的会等本事务结束。
		boardQuery := "SELECT " + boardColumns + " FROM rank_board WHERE board_id=?"
		if shard == nil {
			boardQuery += " FOR UPDATE"
		}
		b, err := scanBoard(tx.QueryRowContext(ctx, boardQuery, in.BoardId))
		if err != nil {
			return err
		}
		if shard == nil {
			// 拿到榜行锁之后再看一次:同一片的另一笔重放可能刚建好片行。
			if shard, err = lockShard(ctx, tx, in.BoardId, in.Generation, in.SourceShard); err != nil {
				return err
			}
		}
		if b.Producer != caller {
			return ErrDenied
		}
		if b.Generation != in.Generation || b.SourceEpoch != in.SourceEpoch || b.DeletedMs != 0 {
			return ErrConflict
		}
		// 游标按片各自连续:幂等键是 (shard, cursor),本片已落账到哪一批记在片行上。
		var committed uint64
		if shard != nil {
			committed = shard.committed
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
		removed, err := removedEntries(ctx, tx, in.BoardId, in.Generation)
		if err != nil {
			return err
		}
		// 一批最多 128 个实体:整批一次读(并锁住已有行)、至多两条语句写(2026-11-05 起;11-11 起不再持榜行锁)。
		// 合并规则不变,在内存里逐个算;同一实体被别的片的批次同时改,靠 loadScores 的行锁串行。
		existing, err := loadScores(ctx, tx, in.BoardId, in.Generation, entries, removed)
		if err != nil {
			return err
		}
		writes := make([]scoreWrite, 0, len(entries))
		var inserted uint64
		for _, entry := range entries {
			// 移除在这一代内是终局。这一批照样推进游标,其他实体照常写。
			if removed[entry.EntityId] {
				continue
			}
			old, found := existing[entry.EntityId]
			// 不同片的批次到达顺序不代表发生顺序:带版本时旧版本迟到直接跳过,否则榜分会退回旧值。
			if found && entry.ScoreVersion != 0 && entry.ScoreVersion <= old.version {
				continue
			}
			next := entry.Score
			if in.Mode == rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT {
				if (next > 0 && old.score > math.MaxInt64-next) || (next < 0 && old.score < math.MinInt64-next) {
					return ErrInvalid
				}
				next = old.score + next
			}
			if in.Mode == rankpb.RankSubmitMode_RANK_SUBMIT_SET_IF_HIGHER && found && ((b.Ascending == 0 && next <= old.score) || (b.Ascending != 0 && next >= old.score)) {
				continue
			}
			// 带版本的生产者自己定 achieved_ms(分数最后一次变化的时刻),和到达顺序无关;不带版本的按「分数变了才更新」。
			achieved := old.achieved
			if !found || next != old.score || entry.ScoreVersion != 0 {
				achieved = entry.AchievedMs
			}
			writes = append(writes, scoreWrite{entity: entry.EntityId, score: next, achieved: achieved, version: entry.ScoreVersion, existed: found})
			if !found {
				inserted++
			}
		}
		if err := writeScores(ctx, tx, b, in.SourceCursor, writes); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO rank_input_receipt (board_id,generation,source_shard,source_cursor,request_id,request_hash,committed_ms) VALUES (?,?,?,?,?,?,?)", in.BoardId, in.Generation, in.SourceShard, in.SourceCursor, in.RequestId, digest, uint64(r.now().UnixMilli())); err != nil {
			return err
		}
		// 本片游标推进到这一批;这一批新上榜的实体记在本片行上。新插入的行都由 writeScores 普通 INSERT 写成
		// (别的片抢先插入时整笔重试),所以 inserted 精确。各片行之和 = 本代已落账的批次总数与现有实体数。
		if err := advanceShard(ctx, tx, in, shard == nil, inserted); err != nil {
			return err
		}
		result.CommittedCursor = in.SourceCursor
		return nil
	})
	return result, err
}

// advanceShard 把本片的片行推进到 in.SourceCursor,并加上这一批新上榜的实体数;create 时本片还没有片行,调用方持榜行锁。
func advanceShard(ctx context.Context, tx *sql.Tx, in *rankpb.SubmitRankScoresRequest, create bool, inserted uint64) error {
	if create {
		_, err := tx.ExecContext(ctx, "INSERT INTO rank_board_shard (board_id,generation,source_shard,committed_cursor,entries) VALUES (?,?,?,?,?)",
			in.BoardId, in.Generation, in.SourceShard, in.SourceCursor, inserted)
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE rank_board_shard SET committed_cursor=?,entries=entries+? WHERE board_id=? AND generation=? AND source_shard=?",
		in.SourceCursor, inserted, in.BoardId, in.Generation, in.SourceShard)
	return err
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
		// 持榜行锁再按片号升序锁全部片行:在途的批次都已提交或还没开始,之后的批次读到封榜状态被拒(shards.go 的锁序)。
		shards, err := lockAllShards(ctx, tx, b.BoardId, b.Generation)
		if err != nil {
			return err
		}
		finalCursor := committedTotal(shards)
		if finalCursor != in.ExpectedSourceCursor {
			return ErrCursor
		}
		// 顺着名次索引取前 N(order_key 已把升降序统一成同一方向,见 orderKey),不整榜排序。
		rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? ORDER BY "+rankIndexOrder+" LIMIT ?", b.BoardId, b.Generation, top)
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO rank_settlement (settlement_id,board_id,generation,source_epoch,final_cursor,request_id,request_hash,winner_count,sealed_ms) VALUES (?,?,?,?,?,?,?,?,?)", settlement, b.BoardId, b.Generation, b.SourceEpoch, finalCursor, in.RequestId, digest, len(winners), now); err != nil {
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
					// 过期时刻在这里写定,之后每次重投原样带上:改配置、换副本、重启都不改变载荷摘要。
					expire := now + uint64(r.rewardMailWindow.Milliseconds())
					if _, err := tx.ExecContext(ctx, "INSERT INTO rank_mail_reward (reward_id,settlement_id,player_id,`rank`,attachments,status,mail_id,next_attempt_ms,lease_until_ms,lease_token,attempts,created_ms,updated_ms,expire_ms) VALUES (?,?,?,?,?,?,0,?,0,0,0,?,?,?)", ids[index], settlement, winner.EntityId, winner.Rank, payload, RewardPending, now, now, now, expire); err != nil {
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
