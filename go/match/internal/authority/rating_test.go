package authority

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	rankpb "proto/rank"

	"match/internal/metrics"
	"match/internal/ratingrule"
)

// 测试里的计分模式数值(评分轨道与榜都按它分开)。
const (
	mode1v1 = uint32(pb.MatchMode_MATCH_MODE_1V1)
	mode5v5 = uint32(pb.MatchMode_MATCH_MODE_5V5)
)

// rankBatchKey 是榜那边的幂等键:同一张榜、同一片、同一游标。
type rankBatchKey struct {
	board  uint64
	shard  uint32
	cursor uint64
}

type rankReceiver struct {
	rankpb.RankAdminClient
	batches     map[rankBatchKey]*rankpb.SubmitRankScoresRequest
	opened      map[uint32]*rankpb.OpenRankGenerationRequest // board_type → 打开请求
	unknownOnce bool
	aheadBy     uint64 // 非 0:模拟别的副本已经投得更远,回「重复」和更大的游标
}

// boardIDFor 是替身榜给一种 board_type 发的 board_id:1V1 是 10,5V5 是 20,PVP_TEAM 是 30。
func boardIDFor(boardType uint32) uint64 { return 10 * uint64(boardType) }

func (r *rankReceiver) OpenRankGeneration(_ context.Context, request *rankpb.OpenRankGenerationRequest, _ ...grpc.CallOption) (*rankpb.OpenRankGenerationResponse, error) {
	if r.opened == nil {
		r.opened = map[uint32]*rankpb.OpenRankGenerationRequest{}
	}
	r.opened[request.GetBoard().GetBoardType()] = proto.Clone(request).(*rankpb.OpenRankGenerationRequest)
	return &rankpb.OpenRankGenerationResponse{BoardId: boardIDFor(request.GetBoard().GetBoardType())}, nil
}
func (r *rankReceiver) SubmitRankScores(_ context.Context, request *rankpb.SubmitRankScoresRequest, _ ...grpc.CallOption) (*rankpb.SubmitRankScoresResponse, error) {
	key := rankBatchKey{request.BoardId, request.SourceShard, request.SourceCursor}
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
		require.NoError(t, s.DB.QueryRow("SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND mode=? AND shard=? AND source_cursor=?", mode1v1, shard, cursor).Scan(&body))
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
		require.NoError(t, s.DB.QueryRow("SELECT produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, shard).Scan(&produced, &committed, &board))
		require.Equal(t, want, produced)
		require.Equal(t, want, committed)
		require.EqualValues(t, 10, board)
	}
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key), "追平之后再跑一轮什么都不发")
	require.Len(t, receiver.batches, 3)

	final, e := s.StopRankSource(ctx, 1, mode1v1)
	require.NoError(t, e)
	require.EqualValues(t, 3, final, "各片最终游标之和")
	untouched := uint64(6402)
	require.NotContains(t, []uint32{rankSourceShard(first), rankSourceShard(other)}, rankSourceShard(untouched))
	late, e := s.ApplyRating(ctx, untouched, &kafkapb.BattleResultEvent{BattleId: untouched, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e, "停写不拦评分,结果消费不能卡住")
	require.True(t, late)
	var produced uint64
	require.NoError(t, s.DB.QueryRow("SELECT produced_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(untouched)).Scan(&produced))
	require.Zero(t, produced, "没打过对局的片同样停写:这局不进榜,游标不动")
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rank_outbox WHERE request_id=?", untouched).Scan(&count))
	require.Zero(t, count, "停写之后的局不写投榜批次")
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rank_source WHERE source_epoch=1 AND mode=?", mode1v1).Scan(&count))
	require.Equal(t, rankSourceShards, count, "停写把本模式所有片都建出来了")
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
		require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(battle)).Scan(&committed))
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
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(battles[0])).Scan(&committed))
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
		require.NoError(t, s.DB.QueryRow("SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND mode=? AND shard=? AND source_cursor=1", mode1v1, rankSourceShard(battle)).Scan(&body))
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
	rating, e := s.Rating(ctx, 1, mode1v1)
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
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(100)).Scan(&committed))
	require.Zero(t, committed)
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.batches, 1)
	require.NoError(t, s.DB.QueryRow("SELECT committed_cursor FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=?", mode1v1, rankSourceShard(100)).Scan(&committed))
	require.Equal(t, uint64(1), committed)
	final, e := s.StopRankSource(ctx, 1, mode1v1)
	require.NoError(t, e)
	require.Equal(t, uint64(1), final)
	unpublished := metrics.RatingUnpublishedValue(pb.MatchMode(mode1v1).String())
	applied, e = s.ApplyRating(ctx, 101, &kafkapb.BattleResultEvent{BattleId: 101, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e, "停写之后的新局评分照记,不报错、不卡结果消费")
	require.True(t, applied)
	rating, e = s.Rating(ctx, 1, mode1v1)
	require.NoError(t, e)
	require.Greater(t, rating, int64(151600), "评分照记:胜方照常涨分")
	var count int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rating_applied WHERE battle_id=101").Scan(&count))
	require.Equal(t, 1, count, "入账标记照写,之后重放回重复")
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_rank_outbox WHERE request_id=101").Scan(&count))
	require.Zero(t, count, "这局不进榜")
	var produced uint64
	require.NoError(t, s.DB.QueryRow("SELECT SUM(produced_cursor) FROM match_rank_source WHERE source_epoch=1 AND mode=?", mode1v1).Scan(&produced))
	require.Equal(t, final, produced, "封榜的批次总数不因停写之后的局变化")
	require.Equal(t, unpublished+1, metrics.RatingUnpublishedValue(pb.MatchMode(mode1v1).String()))
	applied, e = s.ApplyRating(ctx, 101, &kafkapb.BattleResultEvent{BattleId: 101, MatchMode: 3}, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e)
	require.False(t, applied, "停写之后入账的局重放同样回重复")
	// 停写之前已入账的局迟到重放:重复判定在停写检查之前,照样回「重复」,结果消费不会卡在这条上。
	applied, e = s.ApplyRating(ctx, 100, event, 3, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e, "停写之后已入账局的重复投递不得报错")
	require.False(t, applied)
}

// 计分模式集合(ratingrule.Rated)与榜表(rankBoardTypes)必须一一对应:计分模式没有榜,它的结果在提交前被拒、
// 结果消费卡住;不计分的模式登记了榜,是一张永远没人写的死榜。榜类型互不相同,1V1 沿用拆分前那张榜的键。
func TestRankBoardTypesCoverExactlyRatedModes(t *testing.T) {
	seen := map[uint32]pb.MatchMode{}
	for _, value := range pb.MatchMode_value {
		mode := pb.MatchMode(value)
		boardType, ok := RankBoardType(uint32(mode))
		require.Equal(t, ratingrule.Rated(mode), ok, "模式 %s 的计分与榜登记不一致", mode)
		if !ok {
			continue
		}
		require.NotZero(t, boardType, "模式 %s 的 board_type 不能是 0(rank 拒收)", mode)
		_, registered := rankpb.RankBoardType_name[int32(boardType)]
		require.True(t, registered, "模式 %s 的 board_type %d 没在 rank.proto 的 RankBoardType 登记,客户端没有对应常量", mode, boardType)
		other, duplicate := seen[boardType]
		require.False(t, duplicate, "模式 %s 与 %s 共用 board_type %d,两条评分轨道会写进同一张榜", mode, other, boardType)
		seen[boardType] = mode
	}
	for mode := range rankBoardTypes {
		require.True(t, ratingrule.Rated(mode), "榜表里的模式 %s 不计分", mode)
	}
	boardType, ok := RankBoardType(mode1v1)
	require.True(t, ok)
	require.EqualValues(t, 1, boardType, "1V1 沿用拆分前唯一那张评分榜(robot rank-smoke 读 board_type 1)")
}

// 同一名玩家在两种计分模式下各有一条评分轨道:分数、局数、score_version、来源游标、榜都各算各的。
// 两局落在同一片也互不占对方的游标;停写一个模式不影响另一个模式。
func TestRatingTiDBModesKeepSeparateRatingsSourcesAndBoards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const key = "fixture-only-rank-producer-key-0000"
	duelFirst, teamFight, duelSecond := uint64(7100), uint64(7100+rankSourceShards), uint64(7101)
	require.Equal(t, rankSourceShard(duelFirst), rankSourceShard(teamFight), "1V1 与 5V5 各一局落在同一片")
	require.NotEqual(t, rankSourceShard(duelFirst), rankSourceShard(duelSecond))
	apply := func(battle uint64, mode uint32, teamA, teamB []uint64) {
		t.Helper()
		applied, e := s.ApplyRating(ctx, battle, &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: mode}, mode, teamA, teamB, 1)
		require.NoError(t, e)
		require.True(t, applied)
	}
	apply(duelFirst, mode1v1, []uint64{1}, []uint64{2})
	apply(teamFight, mode5v5, []uint64{1}, []uint64{3})
	apply(duelSecond, mode1v1, []uint64{1}, []uint64{2})

	ratingOf := func(player uint64, mode uint32) int64 {
		t.Helper()
		rating, e := s.Rating(ctx, player, mode)
		require.NoError(t, e)
		return rating
	}
	require.EqualValues(t, 151600, ratingOf(1, mode5v5), "5V5 只打过一局:1500 对 1500 赢一局 +16.00")
	require.Greater(t, ratingOf(1, mode1v1), ratingOf(1, mode5v5), "1V1 赢了两局,分数各算各的")
	require.Equal(t, ratingrule.DefaultCenti, ratingOf(2, mode5v5), "玩家 2 没打过 5V5,1V1 输分不影响它")
	require.Equal(t, ratingrule.DefaultCenti, ratingOf(3, mode1v1), "玩家 3 没打过 1V1")
	gamesOf := func(player uint64, mode uint32) uint64 {
		t.Helper()
		var games uint64
		require.NoError(t, s.DB.QueryRow("SELECT games FROM match_rating WHERE player_id=? AND mode=?", player, mode).Scan(&games))
		return games
	}
	require.EqualValues(t, 2, gamesOf(1, mode1v1))
	require.EqualValues(t, 1, gamesOf(1, mode5v5))

	versionOf := func(mode uint32, shard uint32, cursor uint64, player uint64) uint64 {
		t.Helper()
		var body []byte
		require.NoError(t, s.DB.QueryRow("SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND mode=? AND shard=? AND source_cursor=?", mode, shard, cursor).Scan(&body))
		batch := &rankpb.SubmitRankScoresRequest{}
		require.NoError(t, proto.Unmarshal(body, batch))
		for _, entry := range batch.Entries {
			if entry.EntityId == player {
				return entry.ScoreVersion
			}
		}
		t.Fatalf("模式 %d 片 %d 游标 %d 的批次里没有玩家 %d", mode, shard, cursor, player)
		return 0
	}
	shared := rankSourceShard(duelFirst)
	require.EqualValues(t, 1, versionOf(mode1v1, shared, 1, 1), "1V1 第 1 局")
	require.EqualValues(t, 2, versionOf(mode1v1, rankSourceShard(duelSecond), 1, 1), "1V1 第 2 局,跨片也递增")
	require.EqualValues(t, 1, versionOf(mode5v5, shared, 1, 1), "5V5 自己从第 1 局数起,与 1V1 的局数无关")

	receiver := &rankReceiver{batches: map[rankBatchKey]*rankpb.SubmitRankScoresRequest{}}
	require.NoError(t, s.PublishRankOnce(ctx, receiver, key))
	require.Len(t, receiver.opened, 2, "两个模式各开一张榜,没打过的 PVP_TEAM 不开")
	require.EqualValues(t, 1, receiver.opened[1].GetRequestId(), "1V1 的打开请求与拆分前相同")
	require.EqualValues(t, 2, receiver.opened[2].GetRequestId())
	require.Len(t, receiver.batches, 3)
	for _, want := range []rankBatchKey{{boardIDFor(1), shared, 1}, {boardIDFor(1), rankSourceShard(duelSecond), 1}, {boardIDFor(2), shared, 1}} {
		require.Contains(t, receiver.batches, want, "批次投到本模式的榜、本模式的游标")
	}
	boardOf := func(mode uint32) []uint64 {
		t.Helper()
		rows, e := s.DB.Query("SELECT DISTINCT board_id FROM match_rank_source WHERE source_epoch=1 AND mode=?", mode)
		require.NoError(t, e)
		defer rows.Close()
		var boards []uint64
		for rows.Next() {
			var board uint64
			require.NoError(t, rows.Scan(&board))
			boards = append(boards, board)
		}
		require.NoError(t, rows.Err())
		return boards
	}
	require.Equal(t, []uint64{boardIDFor(1)}, boardOf(mode1v1))
	require.Equal(t, []uint64{boardIDFor(2)}, boardOf(mode5v5))

	// 同一局换个模式重放(结果原件变了)是冲突,不会被记进另一条轨道。
	_, e := s.ApplyRating(ctx, duelFirst, &kafkapb.BattleResultEvent{BattleId: duelFirst, MatchMode: mode5v5}, mode5v5, []uint64{1}, []uint64{2}, 1)
	require.Error(t, e)

	// 按模式停写:1V1 封榜的批次总数只算 1V1,5V5 照常入账。
	final, e := s.StopRankSource(ctx, 1, mode1v1)
	require.NoError(t, e)
	require.EqualValues(t, 2, final)
	late, e := s.ApplyRating(ctx, 7102, &kafkapb.BattleResultEvent{BattleId: 7102, MatchMode: mode1v1}, mode1v1, []uint64{1}, []uint64{2}, 1)
	require.NoError(t, e, "1V1 已停写:评分照记、不进榜")
	require.True(t, late)
	var produced uint64
	require.NoError(t, s.DB.QueryRow("SELECT SUM(produced_cursor) FROM match_rank_source WHERE source_epoch=1 AND mode=?", mode1v1).Scan(&produced))
	require.EqualValues(t, 2, produced, "1V1 的封榜批次总数不变")
	apply(7103, mode5v5, []uint64{1}, []uint64{3})
	final, e = s.StopRankSource(ctx, 1, mode5v5)
	require.NoError(t, e)
	require.EqualValues(t, 2, final, "5V5 两局")
}

// 不计分的模式没有评分轨道:读分、入账、停写都直接拒绝,不写任何一行。
func TestRatingTiDBRejectsUnratedModeWithoutSideEffects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pveTeam := uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
	_, e := s.ApplyRating(ctx, 7200, &kafkapb.BattleResultEvent{BattleId: 7200, MatchMode: pveTeam}, pveTeam, []uint64{1}, []uint64{2}, 1)
	require.Error(t, e)
	_, e = s.Rating(ctx, 1, pveTeam)
	require.Error(t, e)
	_, e = s.StopRankSource(ctx, 1, pveTeam)
	require.Error(t, e)
	for _, table := range []string{"match_rating_applied", "match_rating", "match_rank_source", "match_rank_outbox"} {
		var count int
		require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
		require.Zero(t, count, table)
	}
}
