package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"login/internal/svc"
	"shared/generated/pb/table"
	"shared/idsegment"
)

type fakeSegment struct {
	id  uint64
	err error
}

func (f *fakeSegment) Next(context.Context) (uint64, error) { return f.id, f.err }

// CreatePlayer 的发号步骤:号段成功用号段;失败时按 FallbackToSnowflake 决定回退还是
// 用**既有的** kLoginDataSerializeFailed 整体拒绝 —— 任何情况下都不会拿到 0 或自造 id。
func TestMintPlayerID(t *testing.T) {
	segDown := errors.New("idsegment: segment unavailable")
	snowflake := func() (uint64, error) { return 280000000000000001, nil }
	fenced := func() (uint64, error) { return 0, errors.New("player id generator fenced") }

	cases := []struct {
		name    string
		minter  *idsegment.Minter
		wantID  uint64
		wantTip uint32
	}{
		{
			name:   "segment ok",
			minter: &idsegment.Minter{Name: "player_id", Segment: &fakeSegment{id: 77}},
			wantID: 77,
		},
		{
			name:    "segment down, fallback off → fail closed with the existing id-gen tip",
			minter:  &idsegment.Minter{Name: "player_id", Segment: &fakeSegment{err: segDown}},
			wantTip: uint32(table.LoginError_kLoginDataSerializeFailed),
		},
		{
			name:   "segment down, fallback on → snowflake id",
			minter: &idsegment.Minter{Name: "player_id", Segment: &fakeSegment{err: segDown}, Fallback: snowflake},
			wantID: 280000000000000001,
		},
		{
			name:    "segment down, fallback on but snowflake fenced → still fails closed",
			minter:  &idsegment.Minter{Name: "player_id", Segment: &fakeSegment{err: segDown}, Fallback: fenced},
			wantTip: uint32(table.LoginError_kLoginDataSerializeFailed),
		},
		{
			name:   "segment disabled (rollback switch) → snowflake",
			minter: &idsegment.Minter{Name: "player_id", Fallback: snowflake},
			wantID: 280000000000000001,
		},
		{
			name:    "minter not wired → fails closed instead of nil-deref",
			minter:  nil,
			wantTip: uint32(table.LoginError_kLoginDataSerializeFailed),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewCreatePlayerLogic(context.Background(), &svc.ServiceContext{PlayerIDMinter: tc.minter})
			id, tip := l.mintPlayerID("acct")
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
