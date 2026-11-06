package authority

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	kafkapb "proto/contracts/kafka"
	rankpb "proto/rank"
)

// 分片条件:没绑归属 = 全部片(不加条件);一片都不归 = 什么都不读;部分片 = 只读这些片,片号只走绑定参数。
func TestWorkShardFilterKeepsOnlyOwnedShards(t *testing.T) {
	var s Store
	clause, args := s.WorkShardFilter(workFormationRecovery, "battle_id")
	require.Equal(t, "1=1", clause, "没绑归属判定:全部片归本副本,不加条件")
	require.Empty(t, args)

	s.BindWorkOwner(func(string) bool { return false })
	clause, args = s.WorkShardFilter(workFormationRecovery, "battle_id")
	require.Equal(t, "1=0", clause, "一片都不归本副本:什么都不读,交给负责的副本")
	require.Empty(t, args)

	s.BindWorkOwner(func(key string) bool { return key == "formation:3" || key == "formation:17" })
	clause, args = s.WorkShardFilter(workFormationRecovery, "battle_id")
	require.Equal(t, "MOD(battle_id,64) IN(?,?)", clause)
	require.Equal(t, []any{3, 17}, args)

	require.True(t, (*Store)(nil).ownsWork(rankWorkKey(3, 1)), "nil Store 与没绑一样:全部归本副本")
}

func pendingBattleIDs(t *testing.T, s *Store) []uint64 {
	t.Helper()
	pending, err := s.Pending(context.Background())
	require.NoError(t, err)
	ids := make([]uint64, 0, len(pending))
	for _, f := range pending {
		ids = append(ids, f.BattleId)
	}
	return ids
}

// 成局恢复按 battle_id 分片、在 SQL 里过滤:每个副本只看到自己负责的片,别人的积压占不住本副本的名额。
func TestWorkTiDBPendingReturnsOnlyOwnedShards(t *testing.T) {
	s := testStore(t)
	const first, second = uint64(1), uint64(66) // 片 1 与片 2
	exitFormationFixture(t, s, first, true)
	exitFormationFixture(t, s, second, true)
	require.ElementsMatch(t, []uint64{first, second}, pendingBattleIDs(t, s), "没绑归属:全部片")

	s.BindWorkOwner(func(key string) bool { return key == workShardKey(workFormationRecovery, int(second%WorkShards)) })
	require.Equal(t, []uint64{second}, pendingBattleIDs(t, s))

	s.BindWorkOwner(func(string) bool { return false })
	require.Empty(t, pendingBattleIDs(t, s))
}

// 投榜按 (模式, 来源片) 分工:只投本副本负责的片;另一片由负责它的副本投,不分工时全部投。
func TestWorkTiDBRankPublisherSendsOnlyOwnedShards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const first, other = uint64(8801), uint64(8802) // 来源片 33 与 34
	require.NotEqual(t, rankSourceShard(first), rankSourceShard(other))
	for _, battle := range []uint64{first, other} {
		applied, err := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: mode1v1}, mode1v1, []uint64{1}, []uint64{2}, 1)
		require.NoError(t, err)
		require.True(t, applied)
	}
	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}}
	const key = "fixture-only-rank-producer-key-0000"

	s.BindWorkOwner(func(work string) bool { return work == rankWorkKey(mode1v1, rankSourceShard(first)) })
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 1)
	for batch := range receiver.batches {
		require.Equal(t, rankSourceShard(first), batch.shard, "只投本副本负责的片")
	}

	s.BindWorkOwner(nil)
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 2, "不分工时另一片也投出")
	var committed uint64
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(other)).Scan(&committed))
	require.Equal(t, uint64(1), committed)
}
