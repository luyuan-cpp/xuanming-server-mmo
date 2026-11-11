package assetop

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"shared/workshard"
)

// 缺口清单第 25 条(docs/design/assetop-redelivery-sharding-20261110.md):Tick 的到期扫描按 player_id 分片。
// 这里只测 Loop 一侧的接线:绑没绑分工决定走哪个入口、交给 Store 的条件长什么样、gauge 报几片、哪些路径不看归属。
// 条件在真库上的效果在 guild / trade 的 Store 用例里(economy_repo_test.go、asset_op_due_shard_integration_test.go)。

const testWorkKind = "test-assetop"

// ownedDueStore 在 wakeStore 之上实现 OwnedDueLister:记下每次收到的条件与 ListDue 被调的次数。
// 行照 fakeStore 的到期表回,不模拟 SQL 语义,只认一种:恒假条件 1=0 什么都不回(真库判 Impossible WHERE)。
type ownedDueStore struct {
	*wakeStore

	recMu    sync.Mutex
	dueCalls int                // ListDue 被调的次数:绑了分工之后必须一直是 0
	owned    []workshard.Filter // ListDueOwned 每次收到的条件
}

// newOwnedDueStore 建一个「这些行都已到期、也都是玩家 42 的未决行」的 Store:Tick 与唤醒都看得到它们。
func newOwnedDueStore(ops ...Op) *ownedDueStore {
	wake := newWakeStore(ops...)
	wake.due = append([]uint64(nil), wake.pending...)
	return &ownedDueStore{wakeStore: wake}
}

func (s *ownedDueStore) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	s.recMu.Lock()
	s.dueCalls++
	s.recMu.Unlock()
	return s.fakeStore.ListDue(ctx, nowMs, limit)
}

func (s *ownedDueStore) ListDueOwned(ctx context.Context, nowMs uint64, limit int, owned workshard.Filter) ([]uint64, error) {
	s.recMu.Lock()
	s.owned = append(s.owned, owned)
	s.recMu.Unlock()
	if owned.SQL() == "1=0" {
		return nil, nil
	}
	return s.fakeStore.ListDue(ctx, nowMs, limit)
}

func (s *ownedDueStore) recorded() (int, []workshard.Filter) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.dueCalls, append([]workshard.Filter(nil), s.owned...)
}

// 没绑分工:Tick 照旧走 ListDue(扫全部行),不报 assetop_owned_shards —— 没有序列,而不是报 0
// (mail、回收这类不分片的循环扫的是全部行,报 0 会让人以为它们什么都不做)。
func TestTickWithoutWorkOwnerListsEveryShard(t *testing.T) {
	store := newOwnedDueStore(testOp(1))
	loop, m := newTestLoop(t, store, appliedApplier(), nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1))

	if got := loop.Tick(context.Background()); got != 1 {
		t.Fatalf("应处理 1 行,实际 %d", got)
	}
	dueCalls, owned := store.recorded()
	if dueCalls != 1 || len(owned) != 0 {
		t.Fatalf("没绑分工必须走 ListDue:ListDue %d 次、ListDueOwned %d 次", dueCalls, len(owned))
	}
	if got := testutil.CollectAndCount(m.ownedShards); got != 0 {
		t.Fatalf("没绑分工不该有 assetop_owned_shards 序列,实际 %d 条", got)
	}
}

// 绑了分工:Tick 只走 ListDueOwned,条件按 player_id 取模、片号按升序只走绑定参数;gauge 等于本副本负责的片数。
func TestTickWithWorkOwnerListsOnlyOwnedShards(t *testing.T) {
	store := newOwnedDueStore(testOp(1))
	loop, m := newTestLoop(t, store, appliedApplier(), nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1))
	mine := map[string]bool{workshard.ShardKey(testWorkKind, 40): true, workshard.ShardKey(testWorkKind, 3): true}
	if err := loop.BindWorkOwner(testWorkKind, func(key string) bool { return mine[key] }); err != nil {
		t.Fatalf("BindWorkOwner: %v", err)
	}

	if got := loop.Tick(context.Background()); got != 1 {
		t.Fatalf("应处理 1 行,实际 %d", got)
	}
	dueCalls, owned := store.recorded()
	if dueCalls != 0 {
		t.Fatalf("绑了分工之后不得再走 ListDue(扫全部行),实际调了 %d 次", dueCalls)
	}
	if len(owned) != 1 {
		t.Fatalf("一轮 Tick 应调一次 ListDueOwned,实际 %d 次", len(owned))
	}
	if got := owned[0].SQL(); got != "MOD(player_id,64) IN(?,?)" {
		t.Fatalf("分片条件应按 player_id 取模,实际 %q", got)
	}
	if got := fmt.Sprint(owned[0].Args()); got != "[3 40]" {
		t.Fatalf("片号应按升序只走绑定参数,实际 %s", got)
	}
	if got := testutil.ToFloat64(m.ownedShards.WithLabelValues()); got != 2 {
		t.Fatalf("assetop_owned_shards 应为 2,实际 %v", got)
	}
}

// 全部片都归本副本(成员表为空、或只有自己)时不加条件;一片都不归(成员表里没有自己)时条件恒假,一行都不领。
func TestTickWorkOwnerEdgeShards(t *testing.T) {
	for _, tc := range []struct {
		name          string
		owns          func(string) bool
		wantSQL       string
		wantShards    float64
		wantProcessed int
	}{
		{"全部片都归本副本", func(string) bool { return true }, "1=1", 64, 1},
		{"一片都不归本副本", func(string) bool { return false }, "1=0", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newOwnedDueStore(testOp(1))
			applier := appliedApplier()
			loop, m := newTestLoop(t, store, applier, nil)
			loop.Ledger = sqlLedgerFixture(true, testOp(1))
			if err := loop.BindWorkOwner(testWorkKind, tc.owns); err != nil {
				t.Fatalf("BindWorkOwner: %v", err)
			}

			if got := loop.Tick(context.Background()); got != tc.wantProcessed {
				t.Fatalf("应处理 %d 行,实际 %d", tc.wantProcessed, got)
			}
			if got := int(applier.calls.Load()); got != tc.wantProcessed {
				t.Fatalf("应投递 %d 次,实际 %d", tc.wantProcessed, got)
			}
			_, owned := store.recorded()
			if len(owned) != 1 || owned[0].SQL() != tc.wantSQL || len(owned[0].Args()) != 0 {
				t.Fatalf("条件应为 %q 且不带参数,实际 %+v", tc.wantSQL, owned)
			}
			if got := testutil.ToFloat64(m.ownedShards.WithLabelValues()); got != tc.wantShards {
				t.Fatalf("assetop_owned_shards 应为 %v,实际 %v", tc.wantShards, got)
			}
		})
	}
}

// 绑不上的三种:Store 不支持按片列到期行、kind 为空、owns 为 nil。拒绝之后没有半绑的状态,Tick 照旧扫全部行。
func TestBindWorkOwnerRejectsWhatCannotShard(t *testing.T) {
	plain, _ := newTestLoop(t, newFakeStore(testOp(1)), appliedApplier(), nil)
	if err := plain.BindWorkOwner(testWorkKind, func(string) bool { return true }); err == nil {
		t.Fatal("Store 没实现 OwnedDueLister 时必须拒绝:绑了分工却悄悄扫全部行,扩副本时看不出来")
	}

	store := newOwnedDueStore(testOp(1))
	loop, m := newTestLoop(t, store, appliedApplier(), nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1))
	if err := loop.BindWorkOwner("", func(string) bool { return true }); err == nil {
		t.Fatal("kind 为空必须拒绝:归属键不能与别的分片工作混在一起")
	}
	if err := loop.BindWorkOwner(testWorkKind, nil); err == nil {
		t.Fatal("owns 为 nil 必须拒绝")
	}
	if got := loop.Tick(context.Background()); got != 1 {
		t.Fatalf("拒绝绑定之后仍应扫全部行并处理 1 行,实际 %d", got)
	}
	if dueCalls, owned := store.recorded(); dueCalls != 1 || len(owned) != 0 {
		t.Fatalf("拒绝绑定之后应走 ListDue:ListDue %d 次、ListDueOwned %d 次", dueCalls, len(owned))
	}
	if got := testutil.CollectAndCount(m.ownedShards); got != 0 {
		t.Fatalf("没绑上分工不该有 assetop_owned_shards 序列,实际 %d 条", got)
	}
}

// 落地唤醒不看归属:本副本一片都不负责,唤醒照样处理这个玩家的全部未决行(唤醒落在哪个副本由去重键决定,与分片无关)。
func TestWakePlayerIgnoresWorkOwner(t *testing.T) {
	store := newOwnedDueStore(testOp(1), testOp(2))
	applier := appliedApplier()
	loop, _ := newTestLoop(t, store, applier, nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1), testOp(2))
	if err := loop.BindWorkOwner(testWorkKind, func(string) bool { return false }); err != nil {
		t.Fatalf("BindWorkOwner: %v", err)
	}

	if got := loop.Tick(context.Background()); got != 0 {
		t.Fatalf("一片都不负责时 Tick 不该领行,实际 %d", got)
	}
	n, err := loop.WakePlayer(context.Background(), 42)
	if err != nil {
		t.Fatalf("WakePlayer 出错: %v", err)
	}
	if n != 2 || applier.calls.Load() != 2 {
		t.Fatalf("唤醒应处理并投递 2 行,实际处理 %d、投递 %d", n, applier.calls.Load())
	}
}
