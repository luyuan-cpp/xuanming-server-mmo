package idsegment

import (
	"context"
	"errors"
	"testing"
)

// fakeSource 是 Minter 眼里的假号段客户端。
type fakeSource struct {
	id  uint64
	err error
}

func (f *fakeSource) Next(context.Context) (uint64, error) { return f.id, f.err }

func TestMinter_Policy(t *testing.T) {
	segFail := errors.New("segment down")
	fallbackOK := func() (uint64, error) { return 999, nil }
	fallbackFail := func() (uint64, error) { return 0, errors.New("snowflake fenced") }

	cases := []struct {
		name         string
		m            *Minter
		wantID       uint64
		wantErr      error // errors.Is
		wantFallback uint64
	}{
		{
			name:    "no source at all is a wiring error",
			m:       &Minter{Name: "x"},
			wantErr: ErrNoIDSource,
		},
		{
			name:   "segment ok → segment id",
			m:      &Minter{Name: "x", Segment: &fakeSource{id: 42}, Fallback: fallbackOK},
			wantID: 42,
		},
		{
			name:    "segment fails, no fallback → fail closed with the segment error",
			m:       &Minter{Name: "x", Segment: &fakeSource{err: segFail}},
			wantErr: segFail,
		},
		{
			name:         "segment fails, fallback on → fallback id, counted",
			m:            &Minter{Name: "x", Segment: &fakeSource{err: segFail}, Fallback: fallbackOK},
			wantID:       999,
			wantFallback: 1,
		},
		{
			name:         "segment fails, fallback also fails → its error, never 0/ok",
			m:            &Minter{Name: "x", Segment: &fakeSource{err: segFail}, Fallback: fallbackFail},
			wantErr:      nil, // 具体错误来自 fallback,下面只断言非 nil
			wantFallback: 1,
		},
		{
			name:   "segment disabled (nil) → pure fallback, not counted as a fallback event",
			m:      &Minter{Name: "x", Fallback: fallbackOK},
			wantID: 999,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &capLog{}
			tc.m.Logger = log
			id, err := tc.m.Mint(context.Background())
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantID == 0:
				if err == nil {
					t.Fatalf("want an error, got id=%d", id)
				}
			default:
				if err != nil || id != tc.wantID {
					t.Fatalf("id=%d err=%v, want id=%d", id, err, tc.wantID)
				}
			}
			if got := tc.m.Fallbacks(); got != tc.wantFallback {
				t.Fatalf("Fallbacks() = %d, want %d", got, tc.wantFallback)
			}
			if tc.wantFallback > 0 && log.errorsContaining("falling back to snowflake") == 0 {
				t.Fatal("a fallback must be logged")
			}
		})
	}
}

// *Client 必须满足 Source(编译期断言)。
var _ Source = (*Client)(nil)
