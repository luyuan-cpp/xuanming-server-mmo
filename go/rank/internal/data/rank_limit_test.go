package data

import (
	"testing"

	"github.com/stretchr/testify/require"
	rankpb "proto/rank"
)

// 翻页只到第 limit 名为止(2026-11-11,缺口清单第 18 条):页数按 min(总数, limit) 算,最后一页截在第 limit 名,越界页码钳到末页。
func TestPageWindowStopsAtRankLimit(t *testing.T) {
	cases := []struct {
		name                     string
		total, page, size, limit uint64
		want                     rankPage
	}{
		{name: "空榜一页零行", total: 0, page: 1, size: 20, limit: 100, want: rankPage{page: 1, pages: 1, offset: 0, rows: 0}},
		{name: "总数在上限之内与以前相同", total: 45, page: 3, size: 20, limit: 100, want: rankPage{page: 3, pages: 3, offset: 40, rows: 5}},
		{name: "越界页码钳到末页", total: 45, page: 9, size: 20, limit: 100, want: rankPage{page: 3, pages: 3, offset: 40, rows: 5}},
		{name: "页码 0 当第 1 页", total: 45, page: 0, size: 20, limit: 100, want: rankPage{page: 1, pages: 3, offset: 0, rows: 20}},
		{name: "总数超过上限:页数按上限算", total: 1_000_000, page: 1, size: 30, limit: 100, want: rankPage{page: 1, pages: 4, offset: 0, rows: 30}},
		{name: "上限不是页长的整数倍:末页截在第 limit 名", total: 1_000_000, page: 4, size: 30, limit: 100, want: rankPage{page: 4, pages: 4, offset: 90, rows: 10}},
		{name: "深页钳到上限所在的那一页", total: 1_000_000, page: 50_000, size: 30, limit: 100, want: rankPage{page: 4, pages: 4, offset: 90, rows: 10}},
		{name: "上限恰是页长的整数倍", total: 1_000_000, page: 5, size: 20, limit: 100, want: rankPage{page: 5, pages: 5, offset: 80, rows: 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pageWindowFor(tc.total, tc.page, tc.size, tc.limit))
		})
	}
}

// 名次在上限之内填精确值;超过上限或没数出来(0)时 rank 留 0、标 beyond_rank_limit。
func TestRankOrBeyond(t *testing.T) {
	entry := &rankpb.RankEntry{}
	rankOrBeyond(entry, 7, 10)
	require.EqualValues(t, 7, entry.Rank)
	require.False(t, entry.BeyondRankLimit)
	rankOrBeyond(entry, 10, 10)
	require.EqualValues(t, 10, entry.Rank, "恰好第 limit 名仍精确")
	require.False(t, entry.BeyondRankLimit)
	rankOrBeyond(entry, 11, 10)
	require.Zero(t, entry.Rank)
	require.True(t, entry.BeyondRankLimit)
	rankOrBeyond(entry, 0, 10)
	require.Zero(t, entry.Rank, "没数出来的名次按上限之后处理")
	require.True(t, entry.BeyondRankLimit)
}

// 本人位置没数满时,前后邻居的名次由本人名次推出;数满了(capped)本人与邻居都回 0(在上限之后)。
func TestPositionAround(t *testing.T) {
	exact := &rankedEntry{ahead: 4}
	require.EqualValues(t, 5, positionAround(exact, 0))
	require.EqualValues(t, 3, positionAround(exact, -2))
	require.EqualValues(t, 8, positionAround(exact, 3))
	capped := &rankedEntry{ahead: 12, capped: true}
	require.Zero(t, positionAround(capped, 0))
	require.Zero(t, positionAround(capped, -1))
}
