package svc

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"
)

type stubMergedZoneLister struct {
	rows []store.MergedIntoRow
	err  error
}

func (s stubMergedZoneLister) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	return s.rows, s.err
}

// 编译期断言:装配层把 *store.RouteStore 直接当真源。
var _ mergedIntoLister = (*store.RouteStore)(nil)

// 缓存没装配:main 必须拒启,不看 Store.Required。
func TestMergedZonesUnassembled(t *testing.T) {
	if err := (&ServiceContext{}).MergedZonesUnassembled(); err == nil {
		t.Fatal("a missing cache must refuse to start")
	}
	cache := loadMergedZoneCache(stubMergedZoneLister{}, time.Second)
	if cache == nil {
		t.Fatal("cache not assembled")
	}
	if err := (&ServiceContext{MergedZones: cache}).MergedZonesUnassembled(); err != nil {
		t.Fatalf("assembled cache: %v", err)
	}
}

func TestNewMergedZoneCacheWithoutRouteStore(t *testing.T) {
	if cache := newMergedZoneCache(config.Config{}, nil); cache != nil {
		t.Fatal("without a route store there is nothing to read: the cache stays nil")
	}
}

func TestLoadMergedZoneCache(t *testing.T) {
	if cache := loadMergedZoneCache(stubMergedZoneLister{err: errors.New("db down")}, time.Second); cache != nil {
		t.Fatal("a failed first read leaves the cache nil, main refuses to start")
	}
	broken := []store.MergedIntoRow{{SrcZone: 5, DstZone: 6, RunID: "run", MergedAtMs: 1}, {SrcZone: 6, DstZone: 5, RunID: "run", MergedAtMs: 1}}
	if cache := loadMergedZoneCache(stubMergedZoneLister{rows: broken}, time.Second); cache != nil {
		t.Fatal("a cycle at startup leaves the cache nil, main refuses to start")
	}
	cache := loadMergedZoneCache(stubMergedZoneLister{rows: []store.MergedIntoRow{{SrcZone: 5, DstZone: 6, RunID: "run", MergedAtMs: 1}}}, time.Second)
	if cache == nil {
		t.Fatal("cache not assembled")
	}
	var snapshot *placement.MergedZones = cache.Snapshot()
	if got := snapshot.Final(5); got != 6 {
		t.Fatalf("Final(5) = %d, want 6", got)
	}
}
