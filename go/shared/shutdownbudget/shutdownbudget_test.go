package shutdownbudget

import (
	"testing"
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

func TestLongestRequestTimeout(t *testing.T) {
	cases := []struct {
		name string
		conf zrpc.RpcServerConf
		want time.Duration
	}{
		{"server timeout only", zrpc.RpcServerConf{Timeout: 8000}, 8 * time.Second},
		{"a longer method timeout wins", zrpc.RpcServerConf{Timeout: 4000, MethodTimeouts: []zrpc.MethodTimeoutConf{
			{FullMethod: "/svc/Fast", Timeout: time.Second},
			{FullMethod: "/svc/Slow", Timeout: 12 * time.Second},
		}}, 12 * time.Second},
		{"shorter method timeouts do not lower it", zrpc.RpcServerConf{Timeout: 4000, MethodTimeouts: []zrpc.MethodTimeoutConf{
			{FullMethod: "/svc/Fast", Timeout: time.Second},
		}}, 4 * time.Second},
		// Timeout <= 0:go-zero 不装超时拦截器,MethodTimeouts 也不生效,请求没有上界。
		{"no timeout interceptor means unbounded", zrpc.RpcServerConf{Timeout: 0, MethodTimeouts: []zrpc.MethodTimeoutConf{
			{FullMethod: "/svc/Slow", Timeout: 12 * time.Second},
		}}, 0},
		{"negative timeout means unbounded", zrpc.RpcServerConf{Timeout: -1}, 0},
	}
	for _, tc := range cases {
		if got := LongestRequestTimeout(tc.conf); got != tc.want {
			t.Errorf("%s: LongestRequestTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDrainBudget(t *testing.T) {
	cases := []struct {
		name                string
		longest, background time.Duration
		want                time.Duration
	}{
		{"request timeout only", 4 * time.Second, 0, 4 * time.Second},
		{"background drain is added on top", 4 * time.Second, 15 * time.Second, 19 * time.Second},
		// 请求没有上界:盖不住它,但有界的后台收尾照样要等。
		{"unbounded requests still wait for the background drain", 0, 15 * time.Second, 15 * time.Second},
		{"negative values count as none", -time.Second, -time.Second, 0},
	}
	for _, tc := range cases {
		if got := DrainBudget(tc.longest, tc.background); got != tc.want {
			t.Errorf("%s: DrainBudget(%v, %v) = %v, want %v", tc.name, tc.longest, tc.background, got, tc.want)
		}
	}
	// login 的口径:Timeout 4s + EnterGame 后台链收尾 15s → 强退时限 1 + 19 + 1 = 21s,preStop 5s 之后仍在 30s 宽限期内。
	if got := WaitTimeFor(0, 0, DrainBudget(4*time.Second, 15*time.Second)); got != 21*time.Second {
		t.Errorf("login wait time = %v, want 21s", got)
	}
}

func TestWaitTimeFor(t *testing.T) {
	cases := []struct {
		name                         string
		wrapUp, wait, longestRequest time.Duration
		want                         time.Duration
	}{
		// 没配 Shutdown 块:按 go-zero 缺省 1s / 5.5s 推算。
		{"a 4s timeout needs 6s once the slack is counted", 0, 0, 4 * time.Second, 6 * time.Second},
		{"the default wait time already covers a 2s timeout", 0, 0, 2 * time.Second, DefaultWaitTime},
		{"an 8s timeout needs wrap-up + 8s + slack", 0, 0, 8 * time.Second, 10 * time.Second},
		{"a 10s timeout needs 12s", 0, 0, 10 * time.Second, 12 * time.Second},
		// 配了更长的 wrap-up:在途请求从 wrap-up 结束才开始排空,要一起加上。
		{"a configured wrap-up time is added", 3 * time.Second, 0, 8 * time.Second, 12 * time.Second},
		// 只放宽、不收紧。
		{"a longer configured wait time is kept", 0, 30 * time.Second, 8 * time.Second, 30 * time.Second},
		// 请求没有上界:盖不住,保持原值。
		{"unbounded requests leave the default", 0, 0, 0, DefaultWaitTime},
		{"unbounded requests leave a configured wait time", 0, 9 * time.Second, 0, 9 * time.Second},
	}
	for _, tc := range cases {
		if got := WaitTimeFor(tc.wrapUp, tc.wait, tc.longestRequest); got != tc.want {
			t.Errorf("%s: WaitTimeFor(%v, %v, %v) = %v, want %v", tc.name, tc.wrapUp, tc.wait, tc.longestRequest, got, tc.want)
		}
	}
}
