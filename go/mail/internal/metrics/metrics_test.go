package metrics

import (
	"testing"

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
		"mail_claim_short_circuit_total":   1,
		"mail_list_truncated_total":        1,
		"mail_guild_lookup_failures_total": 1,
		"mail_sweep_candidate_rows":        3 * 2, // table × mode
		"mail_sweep_deleted_total":         3,
		"mail_sweep_invalid_rows_total":    3,
		"mail_sweep_last_run_unixtime":     1,
	} {
		assert.Equal(t, want, got[name], "%s 应恰好预建 %d 条序列", name, want)
	}
}

// TestSweepModeLabelsMatchConfig:mode label 的两个取值与 config 的合法模式逐字相同(本包是叶子包,不 import config;
// 只在测试里对一次)。写错一个字母,gauge 就会落在一条没人看的序列上。
func TestSweepModeLabelsMatchConfig(t *testing.T) {
	assert.Equal(t, config.SweepModeReportOnly, SweepModeReportOnly)
	assert.Equal(t, config.SweepModeDelete, SweepModeDelete)
}
