package authority

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	kafkapb "proto/contracts/kafka"
	rankpb "proto/rank"
)

// rankBatchKey 是榜那边的幂等键:同一片同一游标。
type rankBatchKey struct {
	shard  uint32
	cursor uint64
}

type rankReceiver struct {
	rankpb.RankAdminClient
	batches     map[rankBatchKey]*rankpb.SubmitRankScoresRequest
	unknownOnce bool
	aheadBy     uint64 // 非 0:模拟别的副本已经投得更远,回「重复」和更大的游标
}

func (r *rankReceiver) OpenRankGeneration(context.Context, *rankpb.OpenRankGenerationRequest, ...grpc.CallOption) (*rankpb.OpenRankGenerationResponse, error) {
	return &rankpb.OpenRankGenerationResponse{BoardId: 10}, nil
}
func (r *rankReceiver) SubmitRankScores(_ context.Context, request *rankpb.SubmitRankScoresRequest, _ ...grpc.CallOption) (*rankpb.SubmitRankScoresResponse, error) {
	key := rankBatchKey{request.SourceShard, request.SourceCursor}
	if old := r.batches[key]; old != nil {
		a, b := proto.Clone(old).(*rankpb.SubmitRankScoresRequest), proto.Clone(request).(*rankpb.SubmitRankScoresRequest)
		a.Auth = nil
		b.Auth = nil
		if !proto.Equal(a, b) {
			return nil, errors.New("重放载荷不同")
		}
	} else {
		r.batches[key] = proto.Clone(request).(*rankpb.SubmitRankScoresRequest)
	}
	if r.unknownOnce {
		r.unknownOnce = false
		return nil, context.DeadlineExceeded
	}
	return &rankpb.SubmitRankScoresResponse{CommittedCursor: request.SourceCursor + r.aheadBy, Duplicated: r.aheadBy != 0}, nil
}

// 同一玩家的几局落在不同片:每片游标从 1 起互不相干,批次带这名玩家严格递增的版本;
// 投完各片都追平;停写返回各片之和,还没打过对局的片也被挡住。
func TestRatingTiDBShardedSourceCursorsVersionsAndStopTotal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const key = "fixture-only-rank-producer-key-0000"
	first, other, sameShard := uint64(6400), uint64(6401), uint64(6400+rankSourceShards)
	require.Equal(t, rankSourceShard(first), rankSourceShard(sameShard))
	require.NotEqual(t, rankSourceShard(first), rankSourceShard(other))
	for _, battle := range []uint64{first, other, sameShard} {
		applied, e := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
		require.NoError(t, e)
		require.True(t, applied)
	}
	outbox := func(shard uint32, cursor uint64) *rankpb.SubmitRankScoresRequest {
		t.Helper()
		var body []byte
		require.NoError(t, s.DB.QueryRow("SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND shard=? AND source_cursor=?", shard, cursor).Scan(&body))
		batch := &rankpb.SubmitRankScoresRequest{}
		require.NoError(t, proto.Unmarshal(body, batch))
		require.Equal(t, shard, batch.SourceShard)
		require.Equal(t, cursor, batch.SourceCursor)
		return batch
	}
	versionOf := func(batch *rankpb.SubmitRankScoresRequest, player uint64) uint64 {
		t.Helper()
		for _, entry := range batch.Entries {
			if entry.EntityId == player {
				return entry.ScoreVersion
			}
		}
		t.Fatalf("批次里没有玩家 %d", player)
		return 0
	}
	// 版本跟着玩家走,不跟着片走:跨片也严格递增。
	require.EqualValues(t, 1, versionOf(outbox(rankSourceShard(first), 1), 1))
	require.EqualValues(t, 2, versionOf(outbox(rankSourceShard(other), 1), 1))
	require.EqualValues(t, 3, versionOf(outbox(rankSourceShard(sameShard), 2), 1))

	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}}
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 3)
	for shard, want := range map[uint32]uint64{rankSourceShard(first): 2, rankSourceShard(other): 1} {
		var produced, committed, board uint64
		require.NoError(t, s.DB.QueryRow("SELECT produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE source_epoch=1 AND shard=?", shard).Scan(&produced, &committed, &board))
		require.Equal(t, want, produced)
		require.Equal(t, want, committed)
		require.EqualValues(t, 10, board)
	}
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key), "追平之后再跑一轮什么都不发")
	require.Len(t, receiver.batches, 3)

	final, e := s.StopRankSource(ctx, 1)
	require.NoError(t, e)
	require.EqualValues(t, 3, final, "各片最终游标之和")
	untouched := uint64(6402)
	require.NotContains(t, []uint32{rankSourceShard(first), rankSourceShard(other)}, rankSourceShard(untouched))
	_, e = s.ApplyRating(ctx, untouched, &kafkapb.BattleResultEvent{BattleId: untouched, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.Error(t, e, "没打过对局的片同样停写")
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rank_source WHERE source_epoch=1").Scan(&count))
	require.Equal(t, rankSourceShards, count, "停写把所有片都建出来了")
}

// 一片的批次投失败不拦别的片,这一片下一轮从原游标接着投。
func TestRatingTiDBOneShardFailureDoesNotBlockOthers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const key = "fixture-only-rank-producer-key-0000"
	bad, good := uint64(6500), uint64(6501)
	for _, battle := range []uint64{bad, good} {
		_, e := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
		require.NoError(t, e)
	}
	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}, unknownOnce: true}
	// 片号小的先投,第一批回超时;ctx 还活着,第二片照投。
	require.Less(t, rankSourceShard(bad), rankSourceShard(good))
	require.ErrorIs(t, s.PublishRankOnce(ctx, receiver, key), context.DeadlineExceeded)
	committedOf := func(battle uint64) uint64 {
		t.Helper()
		var committed uint64
		require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND shard=?", rankSourceShard(battle)).Scan(&committed))
		return committed
	}
	require.Zero(t, committedOf(bad), "回包未知不推进")
	require.EqualValues(t, 1, committedOf(good), "别的片不受影响")
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.EqualValues(t, 1, committedOf(bad), "下一轮接着投")
}

// 预算只剩一批余量时收手:一批都不发,也不回错误。回错误会让 relay.Hold 永久挡住排空。
func TestRatingTiDBRoundBudgetExhaustedStopsWithoutError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const key = "fixture-only-rank-producer-key-0000"
	_, e := s.ApplyRating(ctx, 6700, &kafkapb.BattleResultEvent{BattleId: 6700, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e)
	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}}
	short, cancel := context.WithTimeout(ctx, rankPublishBatchReserve/2)
	defer cancel()
	require.NoError(t, s.PublishRankOnce(short, receiver, key))
	require.Empty(t, receiver.batches)
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 1, "下一轮接着投")
}

// 发布者没有选主:别的副本已经投得更远时,榜回「重复」和更大的游标。结局已定,不算故障。
func TestRatingTiDBCursorAheadFromOtherReplicaIsNotAFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const key = "fixture-only-rank-producer-key-0000"
	battles := []uint64{6800, 6800 + rankSourceShards, 6800 + 2*rankSourceShards, 6800 + 3*rankSourceShards}
	for _, battle := range battles {
		_, e := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
		require.NoError(t, e)
	}
	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}, aheadBy: 3}
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 1, "榜已经确认到最后一批,不用再投")
	var committed uint64
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND shard=?", rankSourceShard(battles[0])).Scan(&committed))
	require.EqualValues(t, len(battles), committed)
}

// 分数没变的对局不改 achieved_ms:它由来源定,榜上同分决胜不看批次到达顺序。
func TestRatingTiDBAchievedMsFollowsScoreChangeNotGames(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, second := uint64(6900), uint64(6901)
	require.NotEqual(t, rankSourceShard(first), rankSourceShard(second))
	for _, battle := range []uint64{first, second} {
		// 同分两人打平,delta = 0。
		_, e := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 0.5)
		require.NoError(t, e)
	}
	achievedOf := func(battle uint64) uint64 {
		t.Helper()
		var body []byte
		require.NoError(t, s.DB.QueryRow("SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND shard=? AND source_cursor=1", rankSourceShard(battle)).Scan(&body))
		batch := &rankpb.SubmitRankScoresRequest{}
		require.NoError(t, proto.Unmarshal(body, batch))
		for _, entry := range batch.Entries {
			if entry.EntityId == 1 {
				return entry.AchievedMs
			}
		}
		t.Fatalf("批次里没有玩家 1")
		return 0
	}
	require.NotZero(t, achievedOf(first))
	require.Equal(t, achievedOf(first), achievedOf(second))
}

func TestOrderRankShardsByBacklogServesLargestFirst(t *testing.T) {
	shards := []rankShardProgress{{shard: 0, produced: 1}, {shard: 1, produced: 9, committed: 1}, {shard: 2, produced: 9, committed: 1}, {shard: 3, produced: 2, committed: 2}, {shard: 4, produced: 20, committed: 1}}
	orderRankShardsByBacklog(shards)
	var order []uint32
	for _, p := range shards {
		order = append(order, p.shard)
	}
	require.Equal(t, []uint32{4, 1, 2, 0, 3}, order, "积压多的先投,一样多按片号")
}

func TestRatingTiDBTransactionOutboxReplayAndStopBarrier(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	event := &kafkapb.BattleResultEvent{BattleId: 100, MatchMode: 3}
	applied, e := s.ApplyRating(ctx, 100, event, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e)
	require.True(t, applied)
	rating, e := s.Rating(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, int64(151600), rating)
	applied, e = s.ApplyRating(ctx, 100, event, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e)
	require.False(t, applied)
	changed := proto.Clone(event).(*kafkapb.BattleResultEvent)
	changed.BattleConfigId = 7
	_, e = s.ApplyRating(ctx, 100, changed, 3, []uint64{1}, []uint64{2}, 1)
	require.Error(t, e)
	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}, unknownOnce: true}
	const key = "fixture-only-rank-producer-key-0000"
	require.ErrorIs(t, s.PublishRankOnce(ctx, receiver, key), context.DeadlineExceeded)
	var committed uint64
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND shard=?", rankSourceShard(100)).Scan(&committed))
	require.Zero(t, committed)
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 1)
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND shard=?", rankSourceShard(100)).Scan(&committed))
	require.Equal(t, uint64(1), committed)
	final, e := s.StopRankSource(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, uint64(1), final)
	_, e = s.ApplyRating(ctx, 101, &kafkapb.BattleResultEvent{BattleId: 101, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.Error(t, e)
	rating, e = s.Rating(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, int64(151600), rating, "停写拒绝必须回滚评分和入账标记")
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rating_applied WHERE battle_id=101").Scan(&count))
	require.Zero(t, count)
}
