package svc

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/store"
)

type stubMergedZoneLister struct {
	rows []store.MergedIntoRow
	err  error
}

func (s stubMergedZoneLister) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	return s.rows, s.err
}

// db_first 而缓存没装配:main 必须拒启,不看 Store.Required;redis 模式没有合服,不算缺。
func TestMergedZonesUnassembled(t *testing.T) {
	dbFirst := config.Config{RouteStore: config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}}
	redisMode := config.Config{RouteStore: config.RouteStoreConfig{Mode: config.RouteStoreModeRedis}}

	if err := (&ServiceContext{Config: dbFirst}).MergedZonesUnassembled(); err == nil {
		t.Fatal("db_first without the cache must refuse to start")
	}
	if err := (&ServiceContext{Config: dbFirst, MergedZones: routing.NewStaticMergedZoneCache(nil)}).MergedZonesUnassembled(); err != nil {
		t.Fatalf("assembled cache: %v", err)
	}
	if err := (&ServiceContext{Config: redisMode}).MergedZonesUnassembled(); err != nil {
		t.Fatalf("redis mode: %v", err)
	}
}

func TestNewMergedZoneCache(t *testing.T) {
	redisMode := config.Config{RouteStore: config.RouteStoreConfig{Mode: config.RouteStoreModeRedis}}
	cache := newMergedZoneCache(redisMode, nil)
	if cache == nil {
		t.Fatal("redis mode gets an empty static cache")
	}
	if got, err := cache.Resolve(5); err != nil || got != 5 {
		t.Fatalf("Resolve(5) = (%d, %v), want (5, nil)", got, err)
	}

	dbFirst := config.Config{RouteStore: config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}}
	if cache := newMergedZoneCache(dbFirst, nil); cache != nil {
		t.Fatal("db_first without a route store has nothing to read: the cache stays nil")
	}
}

func TestLoadMergedZoneCache(t *testing.T) {
	if cache := loadMergedZoneCache(stubMergedZoneLister{err: errors.New("db down")}, time.Second); cache != nil {
		t.Fatal("a failed first read leaves the cache nil, main refuses to start")
	}
	cache := loadMergedZoneCache(stubMergedZoneLister{rows: []store.MergedIntoRow{{SrcZone: 5, DstZone: 6, RunID: "run", MergedAtMs: 1}}}, time.Second)
	if cache == nil {
		t.Fatal("cache not assembled")
	}
	if got, err := cache.Resolve(5); err != nil || got != 6 {
		t.Fatalf("Resolve(5) = (%d, %v), want (6, nil)", got, err)
	}
}
