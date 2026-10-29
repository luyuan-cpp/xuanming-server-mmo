package svc

// 第 3 批读方门禁的两条启动判据(规格 §13.2 第 2 条):
//   - RouteReadGateUnassembled(M1-5):读方开关开着而 route store 没装配起来 → main 不论 Store.Required 都拒启;
//   - routeReadyProbeNeverArms(A2-11):启动探针的哪些结论要按配置错误记 ERROR(照常装配,不拒启)。
// 两者都是纯判定,不连 Redis、不碰库。

import (
	"strings"
	"testing"

	"data_service/internal/config"
	"shared/placement"
)

// TestRouteReadGateUnassembled:只有「ReadRequiresReady=true 且 PlayerRouteStore 为 nil」回错误,与 Store.Required 无关;
// 开关关着(组件缺省)时 route store 缺不缺都不归它管(那是 MissingStores + Store.Required 的事)。
func TestRouteReadGateUnassembled(t *testing.T) {
	withGate := func(readRequiresReady, storeRequired bool) config.Config {
		var c config.Config
		c.RouteStore = config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, ReadRequiresReady: readRequiresReady}.Normalize()
		c.Store.Required = storeRequired
		return c
	}
	cases := []struct {
		name       string
		ctx        *ServiceContext
		wantRefuse bool
	}{
		{"gate on, route store missing, Store.Required=false", &ServiceContext{Config: withGate(true, false)}, true},
		{"gate on, route store missing, Store.Required=true", &ServiceContext{Config: withGate(true, true)}, true},
		{"gate on, route store assembled", &ServiceContext{Config: withGate(true, false), PlayerRouteStore: stubPlayerRouteStore{}}, false},
		{"gate off, route store missing", &ServiceContext{Config: withGate(false, false)}, false},
		{"gate off, route store assembled", &ServiceContext{Config: withGate(false, true), PlayerRouteStore: stubPlayerRouteStore{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ctx.RouteReadGateUnassembled()
			if tc.wantRefuse {
				if err == nil || !strings.Contains(err.Error(), "ReadRequiresReady=true") {
					t.Fatalf("want a refusal naming ReadRequiresReady, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no refusal, got %v", err)
			}
		})
	}
}

// TestRouteReadyProbeNeverArms:7 个 verdict 里只有 not_master 与 identity_unavailable 是「标记对本实例永远挂不上」的配置错误;
// absent / identity_mismatch / malformed / newer_version / trusted 都是启动时的正常状态。placement 新增 verdict 时这里先红,
// 逼着评审它算不算配置错误。
func TestRouteReadyProbeNeverArms(t *testing.T) {
	want := map[placement.RouteReadyVerdict]bool{
		placement.RouteReadyTrusted:             false,
		placement.RouteReadyAbsent:              false,
		placement.RouteReadyMalformed:           false,
		placement.RouteReadyNewerVersion:        false,
		placement.RouteReadyIdentityUnavailable: true,
		placement.RouteReadyNotMaster:           true,
		placement.RouteReadyIdentityMismatch:    false,
	}
	if n := len(placement.RouteReadyVerdictNames()); n != len(want) {
		t.Fatalf("placement has %d route ready verdicts, this table covers %d: classify the new one", n, len(want))
	}
	for v, never := range want {
		if got := routeReadyProbeNeverArms(v); got != never {
			t.Fatalf("routeReadyProbeNeverArms(%s) = %t, want %t", v, got, never)
		}
	}
}
