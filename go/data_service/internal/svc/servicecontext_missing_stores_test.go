package svc

import (
	"reflect"
	"testing"
)

// MissingStores 是 Store.Required 拒启判定的唯一依据:四个 store 各自独立上报,顺序固定(日志与告警可比对)。
// 这里只需要"是不是 nil",所以直接用零值结构,不连库。
func TestMissingStoresReportsEachNilStore(t *testing.T) {
	all := []string{"snapshot", "transaction_log", "id_segment", "player_name"}
	if got := (&ServiceContext{}).MissingStores(); !reflect.DeepEqual(got, all) {
		t.Fatalf("a context with no stores must report all four in order, got %v", got)
	}
}
