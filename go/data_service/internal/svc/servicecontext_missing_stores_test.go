package svc

import (
	"context"
	"reflect"
	"testing"

	"data_service/internal/store"
	"shared/accountdirectory"
	"shared/serviceauth"
)

// MissingStores 是 Store.Required 拒启判定的唯一依据:各个 store 各自独立上报,顺序固定(日志与告警可比对)。
// 这里只需要"是不是 nil",所以直接用零值结构,不连库。
func TestMissingStoresReportsEachNilStore(t *testing.T) {
	all := []string{"account_directory", "snapshot", "transaction_log", "id_segment", "player_name", "player_route"}
	if got := (&ServiceContext{}).MissingStores(); !reflect.DeepEqual(got, all) {
		t.Fatalf("a context with no stores must report all of them in order, got %v", got)
	}
}

// 下面几个桩只为让 MissingStores 看到"非 nil":嵌入接口本身(零值)即满足接口,方法从不会被调用 ——
// MissingStores 只判空,调到任何方法都是测试写错了,nil 接口会当场 panic 暴露出来。
type stubAccountDirectory struct{ accountdirectory.Directory }
type stubSnapshotStore struct{ SnapshotStore }
type stubTxLogStore struct{ TransactionLogStore }
type stubIdSegmentStore struct{ IdSegmentStore }
type stubPlayerNameStore struct{ PlayerNameStore }

// stubPlayerRouteStore 是 PlayerRouteStore 的最小实现。
type stubPlayerRouteStore struct{}

func (stubPlayerRouteStore) RegisterHome(context.Context, store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	return store.RouteRegisterResult{}, nil
}
func (stubPlayerRouteStore) GetRoutes(context.Context, []uint64) (map[uint64]store.RouteRow, error) {
	return nil, nil
}
func (stubPlayerRouteStore) Close() error { return nil }

// 接口一变,编译期先炸:装配处把 *store.RouteStore 直接当 PlayerRouteStore 用。
var _ PlayerRouteStore = (*store.RouteStore)(nil)

// player_route 与别的 store 一样必需:没有路由表,归属读写都只能 fail-closed。
func TestMissingStoresReportsPlayerRoute(t *testing.T) {
	ready := func(route PlayerRouteStore) *ServiceContext {
		return &ServiceContext{
			SnapshotStore:    stubSnapshotStore{},
			AccountDirectory: stubAccountDirectory{}, AccountDirectoryAuth: &serviceauth.Verifier{},
			TxLogStore:       stubTxLogStore{},
			IdSegmentStore:   stubIdSegmentStore{},
			PlayerNameStore:  stubPlayerNameStore{},
			PlayerRouteStore: route,
		}
	}
	if got := ready(nil).MissingStores(); !reflect.DeepEqual(got, []string{"player_route"}) {
		t.Fatalf("MissingStores() without the route store = %v", got)
	}
	if got := ready(stubPlayerRouteStore{}).MissingStores(); got != nil {
		t.Fatalf("MissingStores() with everything assembled = %v", got)
	}
}
