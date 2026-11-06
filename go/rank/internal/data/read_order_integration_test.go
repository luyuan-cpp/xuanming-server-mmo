package data

import (
	"context"
	"sort"
	"testing"

	rankpb "proto/rank"
)

// 名次读取改走索引之后(缺口清单第 17、18 条),Page / Around / 总数必须与「整榜按规则排序后编号」逐项相同:
// 降序榜与升序榜各一张;分数、达成时刻都有并列,最后按 entity_id 决胜;删一个实体之后总数与名次一起变。
func TestTiDBRankReadsMatchBruteForceOrder(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })

	type fixtureEntry struct {
		entity   uint64
		score    int64
		achieved uint64
	}
	var fixture []fixtureEntry
	var inputs []*rankpb.RankScoreInput
	// entity = i*3 mod 31:30 个互不相同、与插入顺序无关的 id;分数 5 档、达成时刻 3 档,并列很多。
	for i := uint64(1); i <= 30; i++ {
		e := fixtureEntry{entity: i * 3 % 31, score: int64(i%5) * 10, achieved: 100 + i%3}
		fixture = append(fixture, e)
		inputs = append(inputs, &rankpb.RankScoreInput{EntityId: e.entity, Score: e.score, AchievedMs: e.achieved})
	}
	reference := func(ascending bool, removed uint64) []fixtureEntry {
		out := make([]fixtureEntry, 0, len(fixture))
		for _, e := range fixture {
			if e.entity != removed {
				out = append(out, e)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.score != b.score {
				if ascending {
					return a.score < b.score
				}
				return a.score > b.score
			}
			if a.achieved != b.achieved {
				return a.achieved < b.achieved
			}
			return a.entity < b.entity
		})
		return out
	}

	for _, ascending := range []bool{false, true} {
		key := &rankpb.RankBoardKey{BoardType: 7, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "order-desc", Generation: 1}
		if ascending {
			key.Period = "order-asc"
		}
		opened, err := r.Open(ctx, "match", &rankpb.OpenRankGenerationRequest{Board: key, Producer: "match", SourceEpoch: 11, RequestId: next0(t, ctx, next), Ascending: ascending})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Submit(ctx, "match", &rankpb.SubmitRankScoresRequest{BoardId: opened.BoardId, Generation: 1, SourceEpoch: 11, SourceCursor: 1, RequestId: next0(t, ctx, next),
			Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET, Entries: inputs}); err != nil {
			t.Fatal(err)
		}
		checkBoard := func(label string, removed uint64) {
			want := reference(ascending, removed)
			readKey := &rankpb.RankBoardKey{BoardType: key.BoardType, Scope: key.Scope, Period: key.Period}
			for _, size := range []uint32{4, 7} {
				pages := (uint64(len(want)) + uint64(size) - 1) / uint64(size)
				for page := uint32(1); uint64(page) <= pages+1; page++ {
					player := want[len(want)/2].entity
					got, err := r.Page(ctx, player, &rankpb.GetRankPageRequest{Board: readKey, Page: page, PageSize: size})
					if err != nil {
						t.Fatalf("%s ascending=%v Page(%d,%d): %v", label, ascending, page, size, err)
					}
					selected := min(uint64(page), pages)
					if got.TotalCount != uint64(len(want)) || got.PageCount != pages || uint64(got.Page) != selected {
						t.Fatalf("%s ascending=%v Page(%d,%d) totals = %d/%d/%d, want %d/%d/%d", label, ascending, page, size,
							got.TotalCount, got.PageCount, got.Page, len(want), pages, selected)
					}
					start := (selected - 1) * uint64(size)
					end := min(start+uint64(size), uint64(len(want)))
					if uint64(len(got.Entries)) != end-start {
						t.Fatalf("%s ascending=%v Page(%d,%d) has %d entries, want %d", label, ascending, page, size, len(got.Entries), end-start)
					}
					for i, entry := range got.Entries {
						w := want[start+uint64(i)]
						if entry.EntityId != w.entity || entry.Score != w.score || entry.AchievedMs != w.achieved || entry.Rank != start+uint64(i)+1 {
							t.Fatalf("%s ascending=%v Page(%d,%d)[%d] = %+v, want entity %d rank %d", label, ascending, page, size, i, entry, w.entity, start+uint64(i)+1)
						}
					}
					if got.Self.GetEntityId() != player || got.Self.GetRank() != uint64(len(want)/2)+1 {
						t.Fatalf("%s ascending=%v self = %+v, want entity %d rank %d", label, ascending, got.Self, player, len(want)/2+1)
					}
				}
			}
			for _, place := range []int{0, len(want) / 2, len(want) - 1} {
				player := want[place].entity
				got, err := r.Around(ctx, player, &rankpb.GetRankAroundRequest{Board: readKey, Radius: 3})
				if err != nil {
					t.Fatalf("%s ascending=%v Around(%d): %v", label, ascending, player, err)
				}
				lo, hi := max(place-3, 0), min(place+3, len(want)-1)
				if len(got.Entries) != hi-lo+1 || got.Self.GetEntityId() != player || got.Self.GetRank() != uint64(place)+1 {
					t.Fatalf("%s ascending=%v Around(%d) = %d entries self %+v, want %d entries self rank %d", label, ascending, player, len(got.Entries), got.Self, hi-lo+1, place+1)
				}
				for i, entry := range got.Entries {
					if entry.EntityId != want[lo+i].entity || entry.Rank != uint64(lo+i)+1 {
						t.Fatalf("%s ascending=%v Around(%d)[%d] = %+v, want entity %d rank %d", label, ascending, player, i, entry, want[lo+i].entity, lo+i+1)
					}
				}
			}
			outsider, err := r.Around(ctx, 9999, &rankpb.GetRankAroundRequest{Board: readKey})
			if err != nil || len(outsider.Entries) != 0 || outsider.Self != nil {
				t.Fatalf("%s ascending=%v Around for an unranked player = %+v, %v; want empty", label, ascending, outsider, err)
			}
		}
		checkBoard("initial", 0)

		removed := reference(ascending, 0)[2].entity
		if err := r.Remove(ctx, "match", &rankpb.RemoveRankEntryRequest{BoardId: opened.BoardId, Generation: 1, EntityId: removed, RequestId: next0(t, ctx, next)}, false); err != nil {
			t.Fatal(err)
		}
		checkBoard("after remove", removed)
	}
}

func next0(t *testing.T, ctx context.Context, next NextID) uint64 {
	t.Helper()
	id, err := next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
