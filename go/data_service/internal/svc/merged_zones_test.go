package svc

import (
	"errors"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"
)

// 编译期断言:装配层把 *store.RouteStore 直接当真源(副本那一侧的断言在 shared/mergedzonecopy)。
var _ mergedZoneTruth = (*store.RouteStore)(nil)

// 缓存没装配:main 必须拒启,不看 Store.Required。
func TestMergedZonesUnassembled(t *testing.T) {
	if err := (&ServiceContext{}).MergedZonesUnassembled(); err == nil {
		t.Fatal("a missing cache must refuse to start")
	}
	cache := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 0)}, &fakeMergedZoneCopies{}), time.Second)
	if cache == nil {
		t.Fatal("cache not assembled")
	}
	if err := (&ServiceContext{MergedZones: cache}).MergedZonesUnassembled(); err != nil {
		t.Fatalf("assembled cache: %v", err)
	}
}

func TestNewMergedZoneCacheNeedsTheRouteStoreAndTheEtcdCopy(t *testing.T) {
	if cache := newMergedZoneCache(config.Config{}, nil, nil); cache != nil {
		t.Fatal("without a route store there is nothing to read: the cache stays nil")
	}
	if cache := newMergedZoneCache(config.Config{}, &store.RouteStore{}, nil); cache != nil {
		t.Fatal("without the etcd copy a replica cannot publish before answering: the cache stays nil")
	}
}

func TestLoadMergedZoneCache(t *testing.T) {
	if cache := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{err: errShardZeroUnreachable}, &fakeMergedZoneCopies{}), time.Second); cache != nil {
		t.Fatal("shard 0 unreadable and no etcd copy yet: the cache stays nil, main refuses to start")
	}
	denied := errors.New("Error 1146 (42S02): Table 'zone_merge_history' doesn't exist")
	if cache := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{err: denied}, &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 1, 5, 6), revision: 1}), time.Second); cache != nil {
		t.Fatal("shard 0 reachable but failing deterministically: the cache stays nil even with an etcd copy, main refuses to start")
	}
	copies := &fakeMergedZoneCopies{}
	broken := testMergedZoneMapping("g1", 2, 5, 6, 6, 5)
	if cache := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: broken}, copies), time.Second); cache != nil {
		t.Fatal("a cycle at startup leaves the cache nil, main refuses to start")
	}
	if copies.puts != 0 {
		t.Fatal("a broken mapping must not be published")
	}
	cache := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 1, 5, 6)}, copies), time.Second)
	if cache == nil {
		t.Fatal("cache not assembled")
	}
	var snapshot *placement.MergedZones = cache.Snapshot()
	if got := snapshot.Final(5); got != 6 {
		t.Fatalf("Final(5) = %d, want 6", got)
	}
	if copies.puts != 1 || copies.value.Version != 1 {
		t.Fatalf("the version answered at startup must be in the etcd copy first: %+v after %d put(s)", copies.value, copies.puts)
	}
	// 第 0 片读不到的新副本从副本起,拿到的就是别的副本已经回答过的那一版。
	started := loadMergedZoneCache(newTestMergedZoneSource(&fakeMergedZoneTruth{err: errShardZeroUnreachable}, copies), time.Second)
	if started == nil || started.Snapshot().Final(5) != 6 {
		t.Fatal("with shard 0 unreadable the cache starts from the etcd copy")
	}
}
