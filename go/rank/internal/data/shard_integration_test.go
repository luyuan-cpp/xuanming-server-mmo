package data

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	mailpb "proto/mail"
	rankpb "proto/rank"
)

// 同榜写入按来源片并行(2026-11-11,缺口清单第 17 条;docs/design/leaderboard-system.md「2026-11-11 同榜写入按来源片并行」)
// 的真库回归。门控与本包其他 TestTiDB 用例相同(MAIL_RANK_TEST_TIDB;MAIL_RANK_REQUIRE_TESTS=1 时没配就失败)。

// submitUntilAccepted 像生产者那样重投同一批直到落账:并发时 Submit 可能因死锁、锁等待,或别的片抢先插入同一实体而整笔失败
// (r.tx 内部已重试过),重投同一 (片, 游标) 是幂等的。业务拒绝(游标、资格、已封榜、参数、冲突)不重投,原样回错误。
func submitUntilAccepted(ctx context.Context, r *Repo, in *rankpb.SubmitRankScoresRequest) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		if _, err = r.Submit(ctx, "match", in); err == nil {
			return nil
		}
		if errors.Is(err, ErrCursor) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrDenied) || errors.Is(err, ErrClosed) || errors.Is(err, ErrConflict) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

// 8 片并发、每片 5 批,每批给同样 10 个实体各加 1 分(INCREMENT):同一实体被不同片同时改,不得丢更新。
// 之后核各片之和(批次总数、实体数);移除一个实体,计数之和减一;封榜按各片游标之和核齐,封榜之后的批次被拒。
func TestTiDBShardsSubmitInParallelWithoutLostUpdates(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })
	board := testOpen(t, ctx, r)
	const shards, batches, entities = 8, 5, 10
	batch := func(shard uint32, cursor uint64) *rankpb.SubmitRankScoresRequest {
		in := &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceShard: shard, SourceCursor: cursor,
			RequestId: uint64(shard)*1000 + cursor, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT}
		for entity := uint64(1); entity <= entities; entity++ {
			in.Entries = append(in.Entries, &rankpb.RankScoreInput{EntityId: entity, Score: 1, AchievedMs: cursor})
		}
		return in
	}
	var wg sync.WaitGroup
	failures := make(chan error, shards)
	for shard := uint32(0); shard < shards; shard++ {
		wg.Add(1)
		go func(shard uint32) {
			defer wg.Done()
			for cursor := uint64(1); cursor <= batches; cursor++ {
				if err := submitUntilAccepted(ctx, r, batch(shard, cursor)); err != nil {
					failures <- err
					return
				}
			}
		}(shard)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}

	for entity := uint64(1); entity <= entities; entity++ {
		var score int64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT score FROM rank_score WHERE board_id=? AND generation=1 AND entity_id=?", board.BoardId, entity).Scan(&score))
		require.EqualValues(t, shards*batches, score, "entity %d: 不同片同时加分不得丢更新", entity)
	}
	count, err := boardEntries(ctx, db, board.BoardId, 1)
	require.NoError(t, err)
	require.EqualValues(t, entities, count, "各片计数之和 = 现有实体数,每个实体只算一次")
	var cursorSum uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COALESCE(SUM(committed_cursor),0) FROM rank_board_shard WHERE board_id=? AND generation=1", board.BoardId).Scan(&cursorSum))
	require.EqualValues(t, shards*batches, cursorSum)

	// 移除一个实体:各片计数之和减一。
	require.NoError(t, r.Remove(ctx, "match", &rankpb.RemoveRankEntryRequest{BoardId: board.BoardId, Generation: 1, EntityId: 3, RequestId: 9001}, false))
	count, err = boardEntries(ctx, db, board.BoardId, 1)
	require.NoError(t, err)
	require.EqualValues(t, entities-1, count)

	// 封榜按各片游标之和核齐:少报一批被拒,报齐了封榜,结算记下的最终游标就是这个和。
	freeze := &rankpb.FreezeRankGenerationRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, ExpectedSourceCursor: shards*batches - 1, RequestId: 9002, TopN: 3,
		Rewards: []*rankpb.RankRewardTier{{FirstRank: 1, LastRank: 1, Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 10}}}}}}
	_, err = r.Freeze(ctx, "match", freeze)
	require.ErrorIs(t, err, ErrCursor)
	freeze.ExpectedSourceCursor = shards * batches
	sealed, err := r.Freeze(ctx, "match", freeze)
	require.NoError(t, err)
	require.EqualValues(t, 3, sealed.WinnerCount)
	var finalCursor uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT final_cursor FROM rank_settlement WHERE settlement_id=?", sealed.SettlementId).Scan(&finalCursor))
	require.EqualValues(t, shards*batches, finalCursor)
	_, err = r.Submit(ctx, "match", batch(0, batches+1))
	require.ErrorIs(t, err, ErrClosed, "封榜之后的批次被拒")
}

// 已有片行的片写入不等榜行锁:另一个事务一直持着榜行锁(就像封榜正走到一半),已有片行的批次照样落账。
// 本片第一次写入要在榜行锁里建片行,所以会等;锁放开之后照常落账。
func TestTiDBSubmitOnExistingShardDoesNotWaitForBoardLock(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })
	board := testOpen(t, ctx, r)
	batch := func(shard uint32, cursor uint64) *rankpb.SubmitRankScoresRequest {
		return &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceShard: shard, SourceCursor: cursor,
			RequestId: uint64(shard)*1000 + cursor, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET,
			Entries: []*rankpb.RankScoreInput{{EntityId: uint64(shard) + 100, Score: int64(cursor), AchievedMs: cursor}}}
	}
	_, err := r.Submit(ctx, "match", batch(2, 1))
	require.NoError(t, err)

	hold, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = hold.Rollback() }()
	var locked uint64
	require.NoError(t, hold.QueryRowContext(ctx, "SELECT board_id FROM rank_board WHERE board_id=? FOR UPDATE", board.BoardId).Scan(&locked))

	quick, cancelQuick := context.WithTimeout(ctx, 5*time.Second)
	defer cancelQuick()
	_, err = r.Submit(quick, "match", batch(2, 2))
	require.NoError(t, err, "已有片行的批次不等榜行锁")

	blocked, cancelBlocked := context.WithTimeout(ctx, time.Second)
	defer cancelBlocked()
	_, err = r.Submit(blocked, "match", batch(3, 1))
	require.Error(t, err, "本片第一次写入要拿榜行锁建片行,榜行被锁住时不能落账")

	require.NoError(t, hold.Rollback())
	require.NoError(t, submitUntilAccepted(ctx, r, batch(3, 1)), "榜行锁放开之后照常落账")
}
