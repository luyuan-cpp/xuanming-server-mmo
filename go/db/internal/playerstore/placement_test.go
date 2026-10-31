package playerstore

import (
	"context"
	"errors"
	"testing"

	"shared/placement"
)

func TestDecideStorage(t *testing.T) {
	const zone = 3
	record := func(storage uint32, frozen bool) placementSnapshot {
		return placementSnapshot{rec: placement.Record{StorageID: storage, Frozen: frozen}, recPresent: true}
	}
	cases := []struct {
		name     string
		snap     placementSnapshot
		required bool
		want     uint32
		wantErr  error
	}{
		{name: "有记录用记录", snap: record(7, false), want: 7},
		{name: "有记录时 required 不影响", snap: record(7, false), required: true, want: 7},
		{name: "搬库中不能认领", snap: record(7, true), wantErr: ErrStorageFrozen},
		{name: "没记录用 home_zone", snap: placementSnapshot{home: 5, homePresent: true}, want: 5},
		{name: "home_zone 不等于本进程也不算 stale", snap: placementSnapshot{home: 9, homePresent: true}, want: 9},
		{name: "都没有用本进程 zone", snap: placementSnapshot{}, want: zone},
		{name: "required 且没记录要拒绝", snap: placementSnapshot{home: 5, homePresent: true}, required: true, wantErr: ErrPlayerStorageMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideStorage(tc.snap, zone, tc.required)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("storage = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParsePlacementSnapshot(t *testing.T) {
	snap, err := parsePlacementSnapshot([]interface{}{nil, nil})
	if err != nil || snap != (placementSnapshot{}) {
		t.Fatalf("both keys absent: snap=%+v err=%v", snap, err)
	}
	snap, err = parsePlacementSnapshot([]interface{}{"102:3:frozen:run-a", "101"})
	if err != nil || !snap.recPresent || !snap.rec.Frozen || snap.rec.StorageID != 102 || !snap.homePresent || snap.home != 101 {
		t.Fatalf("record and home present: snap=%+v err=%v", snap, err)
	}
	snap, err = parsePlacementSnapshot([]interface{}{nil, ""})
	if err != nil || snap.homePresent {
		t.Fatalf("empty home follows the shared contract (absent): snap=%+v err=%v", snap, err)
	}
	// 畸形必须拒绝,绝不能当缺席。
	for _, vals := range [][]interface{}{
		{"", nil},
		{"102", nil},
		{"0:1", nil},
		{"102:1:moving:run", nil},
		{nil, "abc"},
		{nil, "0"},
		{nil, "1000000"},
	} {
		if _, err := parsePlacementSnapshot(vals); !errors.Is(err, placement.ErrMalformed) {
			t.Errorf("values %v must be malformed, got %v", vals, err)
		}
	}
	if _, err := parsePlacementSnapshot([]interface{}{int64(1), nil}); err == nil {
		t.Error("an unexpected reply type is an error, not an absent key")
	}
	if _, err := parsePlacementSnapshot([]interface{}{nil}); err == nil {
		t.Error("a short reply is an error")
	}
}

type fakeRouteReadyReader struct {
	read placement.RouteReadyRead
	err  error
}

func (f fakeRouteReadyReader) readRouteReady(context.Context, ...string) (placement.RouteReadyRead, error) {
	return f.read, f.err
}

// 副本不可信时不用读到的值,也不把缺键当「没有落点」。
func TestLookupReadyRefusesUntrustedReplica(t *testing.T) {
	router := &storageRouter{zone: 3, ready: fakeRouteReadyReader{read: placement.RouteReadyRead{Values: []interface{}{nil, nil}}}}
	if _, err := router.resolve(context.Background(), 42); !errors.Is(err, ErrRouteNotReady) {
		t.Fatalf("err = %v, want ErrRouteNotReady", err)
	}
}

func TestLookupReadyScriptFailureIsAnError(t *testing.T) {
	failure := errors.New("script failed")
	router := &storageRouter{zone: 3, ready: fakeRouteReadyReader{err: failure}}
	if _, err := router.resolve(context.Background(), 42); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the script failure", err)
	}
}
