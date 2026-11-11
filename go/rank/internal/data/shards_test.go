package data

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	rankpb "proto/rank"
)

// 本代已落账的批次总数是各片游标之和(缺口清单第 17 条)。
func TestCommittedTotalSumsEveryShard(t *testing.T) {
	require.Zero(t, committedTotal(nil))
	require.EqualValues(t, 9, committedTotal([]shardRow{{shard: 0, committed: 2}, {shard: 3, committed: 7}}))
}

// 移除一个实体时从片号最小、计数大于 0 的片行上减一;全部为 0 时报计数与数据不符。
func TestShardToDecrementPicksLowestShardWithEntries(t *testing.T) {
	shard, ok := shardToDecrement([]shardRow{{shard: 1, entries: 0}, {shard: 4, entries: 2}, {shard: 9, entries: 5}})
	require.True(t, ok)
	require.EqualValues(t, 4, shard)
	_, ok = shardToDecrement([]shardRow{{shard: 1}, {shard: 2}})
	require.False(t, ok, "各片计数都是 0 时删到一行,是数据被绕过服务改过")
	_, ok = shardToDecrement(nil)
	require.False(t, ok)
}

// 整笔重试只认三种:死锁、锁等待超时、别的片抢先插入了同一个实体。别的失败(比如唯一键冲突出现在别处)原样上抛。
func TestRetryableTxError(t *testing.T) {
	require.True(t, retryableTxError(fmt.Errorf("%w: %v", errScoreInsertRace, &mysql.MySQLError{Number: 1062})))
	require.True(t, retryableTxError(&mysql.MySQLError{Number: 1213}))
	require.True(t, retryableTxError(fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1205})))
	require.False(t, retryableTxError(&mysql.MySQLError{Number: 1062}), "裸的唯一键冲突不是本条要重试的那一种")
	require.False(t, retryableTxError(errors.New("rpc failed")))
	require.False(t, retryableTxError(nil))
}

// 片号越界在接缝处拒绝,一条 SQL 都不发:封榜、移除要锁住本代全部片行,片数必须有界。
func TestSubmitRejectsSourceShardOutOfRange(t *testing.T) {
	r := New(nil, nil, nil, nil, func(uint32) bool { return true })
	in := &rankpb.SubmitRankScoresRequest{BoardId: 1, Generation: 1, SourceEpoch: 1, SourceShard: maxSourceShards, SourceCursor: 1, RequestId: 1,
		Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET, Entries: []*rankpb.RankScoreInput{{EntityId: 7, Score: 1, AchievedMs: 1}}}
	_, err := r.Submit(context.Background(), "match", in)
	require.ErrorIs(t, err, ErrInvalid)
}
