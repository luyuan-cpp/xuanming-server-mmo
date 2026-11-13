package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"mail/internal/config"
)

// TestPrebuiltZeroSeries 钉住"所有 label 组合都预建 0 值序列"(设计 §4.7 / §10 ⑤):Prometheus 里从未发生过的序列根本不存在,
// increase(...) > 0 一类告警在序列缺失时既不报警也不报错,与"一切正常"长得一样。数字 = 各 label 取值个数的乘积。
func TestPrebuiltZeroSeries(t *testing.T) {
	Register()
	Register() // 幂等:重复注册不 panic
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	got := map[string]int{}
	for _, mf := range families {
		got[mf.GetName()] = len(mf.GetMetric())
	}
	for name, want := range map[string]int{
		"mail_admin_send_total":            3 * 7 * 5, // method × sender_kind × outcome
		"mail_push_total":                  4,
		"mail_guild_lookup_failures_total": 1,
		"mail_sweep_candidate_rows":        3 * 2, // table × mode
		"mail_sweep_deleted_total":         3,
		"mail_sweep_invalid_rows_total":    3,
	} {
		assert.Equal(t, want, got[name], "%s 应恰好预建 %d 条序列", name, want)
	}
	// 2026-11-11 删掉的两个死计数器,不能再注册永远是 0 的序列:
	//   - 缺口清单第 35 条:ListMails 没有「系统有效集撞上限」的路径,截断计数已删;
	//   - M1 的领取短路已被 M2 的资产通道领取取代,短路计数没有调用方。
	for _, removed := range []string{"mail_list_truncated_total", "mail_claim_short_circuit_total"} {
		_, registered := got[removed]
		assert.False(t, registered, "%s 已删", removed)
	}
}

// 清理循环的活性 gauge(缺口清单第 25 条第四批):Register 把它挂进默认注册表,名字是 mail_sweep_last_run_unixtime;
// 不预建,循环写过才有序列、值就是写入的时刻,循环有意停止后删掉序列(canary 不跑清理,/metrics 上不能出现一个 0)。
func TestSweepLiveIsRegisteredAndExistsOnlyWhileTheLoopRuns(t *testing.T) {
	Register()
	exported := func() (float64, bool) {
		families, err := prometheus.DefaultGatherer.Gather()
		require.NoError(t, err)
		for _, mf := range families {
			if mf.GetName() == "mail_sweep_last_run_unixtime" && len(mf.GetMetric()) == 1 {
				return mf.GetMetric()[0].GetGauge().GetValue(), true
			}
		}
		return 0, false
	}
	SweepLive.Stop()
	_, ok := exported()
	assert.False(t, ok, "没写过(或循环已停止)时不该有序列")

	SweepLive.Mark(time.Unix(1234, 0))
	value, ok := exported()
	require.True(t, ok, "写过之后默认注册表里应有这条序列")
	assert.Equal(t, float64(1234), value)

	SweepLive.Stop()
	_, ok = exported()
	assert.False(t, ok, "循环停止之后删掉序列")
}

// TestSweepModeLabelsMatchConfig:mode label 的两个取值与 config 的合法模式逐字相同(本包是叶子包,不 import config;
// 只在测试里对一次)。写错一个字母,gauge 就会落在一条没人看的序列上。
func TestSweepModeLabelsMatchConfig(t *testing.T) {
	assert.Equal(t, config.SweepModeReportOnly, SweepModeReportOnly)
	assert.Equal(t, config.SweepModeDelete, SweepModeDelete)
}
