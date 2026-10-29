package routing

// 启动探针报给日志的结论(route_read.go 的 probeRouteReadyVerdict,A2-11,2026-10-22):本实例身份取不到 / 不是主时,
// 不论标记在不在都如实回报 IdentityUnavailable / NotMaster(启动时标记通常不在,VerifyRouteReady 会先回 Absent 把它盖掉);
// 身份没问题时与 VerifyRouteReady 逐个相同。纯函数,不连 Redis。

import (
	"strings"
	"testing"

	"shared/placement"

	"github.com/stretchr/testify/assert"
)

func TestProbeRouteReadyVerdict(t *testing.T) {
	master := placement.RedisInstanceIdentity{RunID: readyRunID, Role: "master", MasterReplID: readyReplID}
	replica := placement.RedisInstanceIdentity{RunID: readyRunID, Role: "slave", MasterReplID: readyReplID}
	matching := readyMarkerFor(t, readyRunID, readyReplID)
	foreign := readyMarkerFor(t, strings.Repeat("f", 40), readyReplID)

	for _, tc := range []struct {
		name string
		read placement.RouteReadyRead
		want placement.RouteReadyVerdict
		// sameAsVerify:身份没问题的情形,结论必须与读路径的判据(VerifyRouteReady)逐个相同。
		sameAsVerify bool
	}{
		{"master, no marker (normal at startup)", placement.RouteReadyRead{Identity: master}, placement.RouteReadyAbsent, true},
		{"master, matching marker", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: matching, Identity: master}, placement.RouteReadyTrusted, true},
		{"master, marker of another instance", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: foreign, Identity: master}, placement.RouteReadyIdentityMismatch, true},
		{"master, malformed marker", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: "garbage", Identity: master}, placement.RouteReadyMalformed, true},

		{"replica, no marker", placement.RouteReadyRead{Identity: replica}, placement.RouteReadyNotMaster, false},
		{"replica, matching marker", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: matching, Identity: replica}, placement.RouteReadyNotMaster, true},
		{"replica, malformed marker", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: "garbage", Identity: replica}, placement.RouteReadyNotMaster, false},

		{"no run_id, no marker", placement.RouteReadyRead{Identity: placement.RedisInstanceIdentity{Role: "master", MasterReplID: readyReplID}}, placement.RouteReadyIdentityUnavailable, false},
		{"no role, no marker", placement.RouteReadyRead{Identity: placement.RedisInstanceIdentity{RunID: readyRunID, MasterReplID: readyReplID}}, placement.RouteReadyIdentityUnavailable, false},
		{"no master_replid, no marker", placement.RouteReadyRead{Identity: placement.RedisInstanceIdentity{RunID: readyRunID, Role: "master"}}, placement.RouteReadyIdentityUnavailable, false},
		{"no identity at all, matching marker", placement.RouteReadyRead{MarkerPresent: true, MarkerRaw: matching}, placement.RouteReadyIdentityUnavailable, true},
	} {
		got := probeRouteReadyVerdict(tc.read)
		assert.Equal(t, tc.want, got, "%s: got %s", tc.name, got)
		if tc.sameAsVerify {
			verdict, _ := placement.VerifyRouteReady(tc.read)
			assert.Equal(t, verdict, got, "%s: must match VerifyRouteReady when the identity check does not pre-empt it", tc.name)
		}
	}
}
