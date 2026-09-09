package logic

import (
	"context"
	"errors"
	"testing"

	"guild/internal/constants"
	"shared/idsegment"
)

type fakeSegment struct {
	id  uint64
	err error
}

func (f *fakeSegment) Next(context.Context) (uint64, error) { return f.id, f.err }

// CreateGuild 的发号步骤:号段成功用号段;失败时按 FallbackToSnowflake 决定回退还是用
// **既有的** ErrIDGenUnavailable 整体拒绝 —— 任何情况下都不会拿到 0 或自造 id。
//
// CreateGuild 本身要 MySQL(GetPlayerGuildID → repo.CreateGuild 都是权威库事务),
// 这里只覆盖被抽出来的发号步骤。
func TestMintGuildID(t *testing.T) {
	segDown := errors.New("idsegment: segment unavailable")
	snowflake := func() (uint64, error) { return 67000000000000001, nil }
	fenced := func() (uint64, error) { return 0, errors.New("snowflake fenced") }

	cases := []struct {
		name    string
		ids     IDMinter
		wantID  uint64
		wantTip uint32
	}{
		{
			name:   "segment ok",
			ids:    &idsegment.Minter{Name: "guild_id", Segment: &fakeSegment{id: 77}},
			wantID: 77,
		},
		{
			name:    "segment down, fallback off → fail closed with ErrIDGenUnavailable",
			ids:     &idsegment.Minter{Name: "guild_id", Segment: &fakeSegment{err: segDown}},
			wantTip: constants.ErrIDGenUnavailable,
		},
		{
			name:   "segment down, fallback on → snowflake id",
			ids:    &idsegment.Minter{Name: "guild_id", Segment: &fakeSegment{err: segDown}, Fallback: snowflake},
			wantID: 67000000000000001,
		},
		{
			name:    "segment down, fallback on but snowflake fenced → still fails closed",
			ids:     &idsegment.Minter{Name: "guild_id", Segment: &fakeSegment{err: segDown}, Fallback: fenced},
			wantTip: constants.ErrIDGenUnavailable,
		},
		{
			name:   "segment disabled (rollback switch) → snowflake",
			ids:    &idsegment.Minter{Name: "guild_id", Fallback: snowflake},
			wantID: 67000000000000001,
		},
		{
			name:    "minter not wired → fails closed instead of nil-deref",
			ids:     nil,
			wantTip: constants.ErrIDGenUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewGuildLogic(nil, tc.ids, nil, nil)
			id, tip := l.mintGuildID(context.Background(), 1001)
			if tc.wantTip != 0 {
				if tip == nil || tip.Id != tc.wantTip {
					t.Fatalf("tip = %v, want id %d", tip, tc.wantTip)
				}
				if id != 0 {
					t.Fatalf("id = %d alongside a failure tip, must be 0", id)
				}
				return
			}
			if tip != nil {
				t.Fatalf("unexpected tip %v", tip)
			}
			if id != tc.wantID {
				t.Fatalf("id = %d, want %d", id, tc.wantID)
			}
		})
	}
}
