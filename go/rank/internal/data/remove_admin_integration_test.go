package data

import (
	"errors"
	"testing"

	rankpb "proto/rank"
)

// 后台(调用方 admin,经双人复核批准)可以删任意榜上的单个条目;整代隐藏仍只许开榜方;
// 既不是开榜方也不是后台的调用方什么都删不了(docs/design/admin-identity-and-dual-control.md §11.2)。
func TestTiDBAdminRemovesAnEntryFromABoardItDidNotOpenButCannotHideIt(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })
	board := testOpen(t, ctx, r)
	first := &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceShard: 1, SourceCursor: 1, RequestId: 10,
		Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET, Entries: []*rankpb.RankScoreInput{{EntityId: 7, Score: 100, AchievedMs: 10, ScoreVersion: 5}}}
	if _, err := r.Submit(ctx, "match", first); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, "guild", &rankpb.RemoveRankEntryRequest{BoardId: board.BoardId, Generation: 1, EntityId: 7, RequestId: 31}, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("非开榜方、非后台的删除 = %v, want ErrDenied", err)
	}
	if err := r.Remove(ctx, AdminCaller, &rankpb.RemoveRankEntryRequest{BoardId: board.BoardId, Generation: 1, RequestId: 32}, true); !errors.Is(err, ErrDenied) {
		t.Fatalf("后台整代隐藏别人开的榜 = %v, want ErrDenied", err)
	}
	if err := r.Remove(ctx, AdminCaller, &rankpb.RemoveRankEntryRequest{BoardId: board.BoardId, Generation: 1, EntityId: 7, RequestId: 33}, false); err != nil {
		t.Fatalf("后台删 match 榜上的条目: %v", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rank_score WHERE board_id=? AND generation=1 AND entity_id=7", board.BoardId).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("删除后还剩 %d 行", left)
	}
}
