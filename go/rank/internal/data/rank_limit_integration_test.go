package data

import (
	"testing"

	"github.com/stretchr/testify/require"
	rankpb "proto/rank"
)

// 精确名次上限的真库回归(2026-11-11,缺口清单第 18 条)。门控与本包其他 TestTiDB 用例相同(MAIL_RANK_TEST_TIDB)。
// 用例把上限调成 3、榜上放 6 人(降序榜,实体 k 的分数 = 700 − 100k,所以实体 k 就是第 k 名):
// 翻页只到第 3 名为止;第 3 名之后的本人与附近条目 rank 为 0、带 beyond_rank_limit;总数仍是真实的 6。
func TestTiDBRankReadsStopCountingAtRankLimit(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })
	r.rankLimit = 3
	board := testOpen(t, ctx, r)
	in := &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceShard: 0, SourceCursor: 1, RequestId: 10,
		Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET}
	for entity := uint64(1); entity <= 6; entity++ {
		in.Entries = append(in.Entries, &rankpb.RankScoreInput{EntityId: entity, Score: int64(700 - 100*entity), AchievedMs: 1})
	}
	_, err := r.Submit(ctx, "match", in)
	require.NoError(t, err)
	key := &rankpb.RankBoardKey{BoardType: 1, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "season-1", Generation: 1}
	ranks := func(entries []*rankpb.RankEntry) (out [][2]uint64) {
		for _, e := range entries {
			out = append(out, [2]uint64{e.EntityId, e.Rank})
		}
		return out
	}

	// 第 1 页:精确的第 1、2 名;页数按上限算(3 人、每页 2 人 → 2 页);本人(第 5 名)在上限之后。
	page, err := r.Page(ctx, 5, &rankpb.GetRankPageRequest{Board: key, Page: 1, PageSize: 2})
	require.NoError(t, err)
	require.EqualValues(t, 6, page.TotalCount, "总数仍是真实人数")
	require.EqualValues(t, 3, page.RankLimit)
	require.EqualValues(t, 2, page.PageCount, "只翻到第 3 名为止")
	require.Equal(t, [][2]uint64{{1, 1}, {2, 2}}, ranks(page.Entries))
	require.EqualValues(t, 5, page.Self.EntityId)
	require.Zero(t, page.Self.Rank)
	require.True(t, page.Self.BeyondRankLimit, "第 5 名排在上限之后")

	// 末页截在第 3 名;越界页码钳到末页。
	for _, asked := range []uint32{2, 9} {
		page, err = r.Page(ctx, 5, &rankpb.GetRankPageRequest{Board: key, Page: asked, PageSize: 2})
		require.NoError(t, err)
		require.EqualValues(t, 2, page.Page)
		require.Equal(t, [][2]uint64{{3, 3}}, ranks(page.Entries), "末页只到第 3 名")
	}

	// 恰好第 3 名的本人不在这一页时,名次照样精确。
	page, err = r.Page(ctx, 3, &rankpb.GetRankPageRequest{Board: key, Page: 1, PageSize: 2})
	require.NoError(t, err)
	require.EqualValues(t, 3, page.Self.Rank)
	require.False(t, page.Self.BeyondRankLimit)

	// 附近:第 5 名、半径 1,本人前面数满 3 + 1 = 4 行 → 本人与前后邻居都在上限之后。
	around, err := r.Around(ctx, 5, &rankpb.GetRankAroundRequest{Board: key, Radius: 1})
	require.NoError(t, err)
	require.EqualValues(t, 3, around.RankLimit)
	require.Len(t, around.Entries, 3)
	for _, e := range around.Entries {
		require.Zero(t, e.Rank, "entity %d", e.EntityId)
		require.True(t, e.BeyondRankLimit, "entity %d", e.EntityId)
	}
	require.Equal(t, []uint64{4, 5, 6}, []uint64{around.Entries[0].EntityId, around.Entries[1].EntityId, around.Entries[2].EntityId})

	// 附近:第 4 名、半径 2,前面只有 3 人(没数满 3 + 2)→ 本人名次精确为 4,前面第 2、3 名照常,本人与后面的在上限之后。
	around, err = r.Around(ctx, 4, &rankpb.GetRankAroundRequest{Board: key, Radius: 2})
	require.NoError(t, err)
	require.Equal(t, [][2]uint64{{2, 2}, {3, 3}, {4, 0}, {5, 0}, {6, 0}}, ranks(around.Entries))
	for _, e := range around.Entries {
		require.Equal(t, e.EntityId >= 4, e.BeyondRankLimit, "entity %d", e.EntityId)
	}
}
