package idsegment

import (
	"context"
	"errors"
	"testing"
)

type fakeSource struct {
	id  uint64
	err error
}

func (f *fakeSource) Next(context.Context) (uint64, error) { return f.id, f.err }

func TestMinter_Mint(t *testing.T) {
	segFail := errors.New("segment unavailable")

	cases := []struct {
		name    string
		m       *Minter
		wantID  uint64
		wantErr error
	}{
		{
			name:   "号段成功",
			m:      &Minter{Name: "x", Segment: &fakeSource{id: 42}},
			wantID: 42,
		},
		{
			name:    "号段失败:本次操作整体失败,不回退",
			m:       &Minter{Name: "x", Segment: &fakeSource{err: segFail}},
			wantErr: segFail,
		},
		{
			name:    "没有号段来源",
			m:       &Minter{Name: "x"},
			wantErr: ErrNoIDSource,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.m.Mint(context.Background())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if id != 0 {
					t.Fatalf("id = %d on failure, must be 0", id)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("id=%d err=%v, want id=%d", id, err, tc.wantID)
			}
		})
	}
}
