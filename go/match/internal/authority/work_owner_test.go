package authority

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	kafkapb "proto/contracts/kafka"
	inboxpb "proto/player_inbox"
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

// 终局发件箱按片分工(缺口清单第 25 条,docs/design/assetop-redelivery-sharding-20261110.md §2.5):整局结果按 battle_id、
// 个人结算按 player_id(首投与等待两页共用一个键),条件在 SQL 的 LIMIT 之前。本副本只投自己负责的片,别的片原样留给
// 负责它的副本;一片都不负责时什么都不投;不分工时其余行投出,已投的不再投。
func TestWorkTiDBOutcomeOutboxDeliversOnlyOwnedShards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const (
		ownedBattle, otherBattle = uint64(64*130 + 5), uint64(64*130 + 6) // 结果片 5 与片 6
		firstBattle, waitBattle  = uint64(64*131 + 9), uint64(64*132 + 9) // 个人结算所在的两局(首投、等待)
		ownedPlayer, otherPlayer = uint64(64*140 + 7), uint64(64*140 + 8) // 个人片 7 与片 8
	)
	insertDueResult(t, s, ownedBattle)
	insertDueResult(t, s, otherBattle)
	for _, player := range []uint64{ownedPlayer, otherPlayer} {
		insertDueSource(t, s, firstBattle, player, false)
		insertDueSource(t, s, waitBattle, player, true)
	}
	inbox := &lockedInbox{inner: &terminalInboxFixture{state: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED}}
	resolve := func(context.Context) (inboxpb.PlayerInboxClient, error) { return inbox, nil }
	var mu sync.Mutex
	published := map[uint64]int{}
	publish := func(_ context.Context, battle uint64, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		published[battle]++
		return nil
	}
	publishedSnapshot := func() map[uint64]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[uint64]int, len(published))
		for battle, n := range published {
			out[battle] = n
		}
		return out
	}
	round := func() {
		t.Helper()
		require.NoError(t, s.PublishBattleOutcomesOnce(ctx, resolve, terminalFixtureSecret, publish))
	}

	s.BindWorkOwner(func(key string) bool {
		return key == workShardKey(workOutcomeResult, int(ownedBattle%WorkShards)) ||
			key == workShardKey(workOutcomeSettlement, int(ownedPlayer%WorkShards))
	})
	round()
	require.Equal(t, map[uint64]int{ownedBattle: 1}, publishedSnapshot(), "结果只投本副本负责的 battle 片")
	require.Equal(t, map[uint64]int{ownedPlayer: 2}, inbox.seenPerPlayer(), "个人结算的首投与等待两页都只投本副本负责的玩家片")
	require.Equal(t, 1, countRows(t, s, "SELECT COUNT(*) FROM match_battle_completion WHERE result_published=0"))
	require.Equal(t, 2, countRows(t, s, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE dispatch_state=1 AND player_id=?", otherPlayer),
		"别的片原样留给负责它的副本")

	s.BindWorkOwner(func(string) bool { return false })
	round()
	require.Len(t, publishedSnapshot(), 1, "一片都不负责:什么都不投")
	require.Equal(t, map[uint64]int{ownedPlayer: 2}, inbox.seenPerPlayer())

	s.BindWorkOwner(nil)
	round()
	require.Equal(t, map[uint64]int{ownedBattle: 1, otherBattle: 1}, publishedSnapshot(), "不分工时另一片也投出,已投的不再投")
	require.Equal(t, map[uint64]int{ownedPlayer: 2, otherPlayer: 2}, inbox.seenPerPlayer())
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_completion WHERE result_published=0"))
	require.Zero(t, countRows(t, s, "SELECT COUNT(*) FROM match_battle_settlement_source WHERE dispatch_state=1"))
}
