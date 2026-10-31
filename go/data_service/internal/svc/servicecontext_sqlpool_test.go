package svc

import (
	"database/sql"
	"testing"

	"data_service/internal/metrics"
)

// fakeStatsOf 按桩的类型给出互不相同的池上限,让用例能核对「哪个字段报成了哪个类别」;nil 与未知类型 ok=false
// (与 store.DBStatsOf 对未装配 store 的语义一致)。桩类型定义在 servicecontext_missing_stores_test.go。
func fakeStatsOf(v any) (sql.DBStats, bool) {
	maxOpen := 0
	switch v.(type) {
	case stubSnapshotStore:
		maxOpen = 1
	case stubTxLogStore:
		maxOpen = 2
	case stubPlayerNameStore:
		maxOpen = 4
	case stubPlayerRouteStore:
		maxOpen = 5
	default:
		return sql.DBStats{}, false
	}
	return sql.DBStats{MaxOpenConnections: maxOpen}, true
}

// 没装配起来的 store(nil 接口)不报样本;nil ServiceContext 与只有假实现的 context 走真 DBStatsOf 时也不报、不 panic。
func TestSQLPoolSamplesSkipsUnassembledStores(t *testing.T) {
	sc := &ServiceContext{
		SnapshotStore:   stubSnapshotStore{},
		PlayerNameStore: stubPlayerNameStore{},
	}
	got := map[string]int{}
	for _, s := range sqlPoolSamples(sc.namedStores(), fakeStatsOf) {
		got[s.Pool] = s.Stats.MaxOpenConnections
	}
	if len(got) != 2 || got[metrics.PoolSnapshot] != 1 || got[metrics.PoolPlayerName] != 4 {
		t.Fatalf("samples = %v, want only snapshot and player_name (the other three stores are nil)", got)
	}

	if samples := sc.SQLPoolSamples(); len(samples) != 0 {
		t.Fatalf("fake stores must not match store.DBStatsOf, got %d samples", len(samples))
	}
	var nilCtx *ServiceContext
	if samples := nilCtx.SQLPoolSamples(); samples != nil {
		t.Fatalf("a nil ServiceContext must report no pools, got %v", samples)
	}
}

// 五个 store 各报成自己的类别(一个 store 一个池),类别名与告警 / 面板约定的取值一致。
func TestSQLPoolSamplesNamesPoolsByStore(t *testing.T) {
	sc := &ServiceContext{
		SnapshotStore:    stubSnapshotStore{},
		TxLogStore:       stubTxLogStore{},
		IdSegmentStore:   stubIdSegmentStore{},
		PlayerNameStore:  stubPlayerNameStore{},
		PlayerRouteStore: stubPlayerRouteStore{},
	}
	got := map[string]int{}
	for _, s := range sqlPoolSamples(sc.namedStores(), fakeStatsOf) {
		if _, dup := got[s.Pool]; dup {
			t.Fatalf("pool %q reported twice", s.Pool)
		}
		got[s.Pool] = s.Stats.MaxOpenConnections
	}
	want := map[string]int{
		"snapshot":        1,
		"transaction_log": 2,
		"player_name":     4,
		"player_route":    5,
	}
	if len(got) != len(want) {
		t.Fatalf("samples = %v, want %v", got, want)
	}
	for pool, maxOpen := range want {
		if got[pool] != maxOpen {
			t.Fatalf("pool %q max open = %d, want %d (all: %v)", pool, got[pool], maxOpen, got)
		}
	}
}
