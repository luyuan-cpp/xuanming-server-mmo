package main

// rank-smoke 场景里不依赖服务端的部分:「在不在榜上」的判定。

import (
	"testing"

	rankpb "proto/rank"
)

// 排在精确名次上限之后的人 rank 为 0、beyond_rank_limit 为 true,仍算在榜上(缺口清单第 18 条);
// 不在榜上是 self 为空,或 rank 为 0 且没有 beyond_rank_limit。
func TestRankSmokeOnBoardCountsEntriesBeyondTheLimit(t *testing.T) {
	cases := []struct {
		name  string
		entry *rankpb.RankEntry
		want  bool
	}{
		{name: "self 为空", entry: nil, want: false},
		{name: "没上榜", entry: &rankpb.RankEntry{}, want: false},
		{name: "精确名次", entry: &rankpb.RankEntry{Rank: 3}, want: true},
		{name: "上限之后", entry: &rankpb.RankEntry{BeyondRankLimit: true}, want: true},
	}
	for _, tc := range cases {
		if got := rankSmokeOnBoard(tc.entry); got != tc.want {
			t.Errorf("%s: rankSmokeOnBoard = %v, want %v", tc.name, got, tc.want)
		}
	}
}
