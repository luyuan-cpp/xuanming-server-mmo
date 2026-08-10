package logic

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"player_locator/internal/svc"
	"shared/safego"
)

// safegoPanicCount 读出 safego 兜住的 panic 计数(safego_panic_total{point=...})。
// safego 用 prometheus.Register 注册到默认 registry,所以从 DefaultGatherer 就能读到;
// 点位不存在时返回 0(第一次 panic 之前该 series 还没被创建)。
func safegoPanicCount(t *testing.T, point string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != "safego_panic_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "point" && l.GetValue() == point {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestLeaseMonitorRoundPanicDoesNotStopLoop 钉死「单轮 panic 不掀掉租约监视循环」。
//
// 背景:StartLeaseMonitor 原本是 main 里一个裸 `go` 起的 for-select,轮体
// processExpiredLeases 也是裸调。轮体一旦 panic:
//   - 裸 `go` 写法下,整个 player_locator 进程当场退出;
//   - 就算外面套一层 recover,循环也会从此永久停摆 —— 进程还活着、日志不再有新行,
//     所有断线玩家的会话从此没人清理,是最难被发现的一类故障。
//
// 构造 panic 的方式:ServiceContext.RedisClient 留 nil,claimExpiredLeases 里
// 对 nil *redis.Client 发 Eval 必然空指针。
//
// 判据:safego_panic_total{point="player_locator.lease_monitor.round"} 能涨到 >= 2。
// 「存在第二轮」本身就证明第一轮的 panic 没有掀掉循环。
func TestLeaseMonitorRoundPanicDoesNotStopLoop(t *testing.T) {
	const roundPoint = "player_locator.lease_monitor.round"
	before := safegoPanicCount(t, roundPoint)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 外层也走 safego.Go:既贴合 main 里的真实启动方式,又保证即使循环真被掀掉,
	// panic 也不会打死测试二进制 —— 那样才能干净地断言失败,而不是整个 go test 崩掉。
	safego.Go("test.lease_monitor_supervisor", func() {
		StartLeaseMonitor(ctx, &svc.ServiceContext{}, 5*time.Millisecond, 1)
	})

	require.Eventually(t, func() bool {
		return safegoPanicCount(t, roundPoint) >= before+2
	}, 5*time.Second, 10*time.Millisecond,
		"LeaseMonitor 在一轮 panic 后停摆:点位 %s 的计数没有继续增长", roundPoint)
}
