package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// 登录取归属 zone 与账号目录的三组直方图(开服线 X-3,L-14 / L-18 压测口径)。
// 压测汇总脚本按 rpc × result 拆列,告警用 increase(..._count{result="error"}[...]) > 0:
// 序列在第一次打点之前不存在的话,汇总列整列 n/a、告警漏掉第一次,所以封闭集合必须在构造 server 时全部预建。

// TestLoginReadClosedSets 钉住 label 取值本身:它们是汇总脚本与告警表达式的契约,改名必须同时改两边。
func TestLoginReadClosedSets(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"homeZoneRPCs", homeZoneRPCs, []string{"home", "batch"}},
		{"homeZoneReadResults", homeZoneReadResults, []string{"ok", "not_found", "error"}},
		{"accountDirectoryRPCs", accountDirectoryRPCs, []string{"read", "create", "lookup", "repair", "remove"}},
		{"accountDirectoryResults", accountDirectoryResults, []string{"ok", "not_found", "rejected", "error"}},
	} {
		if len(tc.got) != len(tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
		for i := range tc.got {
			if tc.got[i] != tc.want[i] {
				t.Fatalf("%s[%d] = %q, want %q", tc.name, i, tc.got[i], tc.want[i])
			}
		}
	}
}

// TestPrimeLoginReadsCreatesAllSeries:PrimeLoginReads 之后 home_zone_read_seconds 6 条、account_directory_rpc_seconds 20 条
// 全部存在,且没有封闭集合之外的组合;home_zone_batch_ids 无 label,注册即导出一条。
// 只断言存在与形状,不断言计数为 0:同一进程里其余用例(或 -count=N 的上一轮)可能已经打过点。
func TestPrimeLoginReadsCreatesAllSeries(t *testing.T) {
	PrimeLoginReads()
	PrimeLoginReads() // 可重复调用

	for _, tc := range []struct {
		family  string
		rpcs    []string
		results []string
		count   int
	}{
		{"data_service_home_zone_read_seconds", homeZoneRPCs, homeZoneReadResults, 6},
		{"data_service_account_directory_rpc_seconds", accountDirectoryRPCs, accountDirectoryResults, 20},
	} {
		want := map[string]bool{}
		for _, rpc := range tc.rpcs {
			for _, result := range tc.results {
				want[rpc+"/"+result] = true
			}
		}
		if len(want) != tc.count {
			t.Fatalf("%s closed set lists %d combinations, want %d", tc.family, len(want), tc.count)
		}
		series := gatherFamily(t, tc.family)
		if len(series) != tc.count {
			t.Fatalf("%s has %d series after priming, want %d", tc.family, len(series), tc.count)
		}
		for _, m := range series {
			key := labelKey(m, "rpc", "result")
			if !want[key] {
				t.Fatalf("%s{%s} is not in the closed set", tc.family, key)
			}
			if m.GetHistogram() == nil {
				t.Fatalf("%s{%s} is not a histogram", tc.family, key)
			}
		}
	}
	if m := gatherFamily(t, "data_service_home_zone_batch_ids"); len(m) != 1 || len(m[0].GetLabel()) != 0 {
		t.Fatalf("data_service_home_zone_batch_ids = %v, want exactly one unlabeled series", m)
	}
}

// TestObserveLoginReadsLandOnTheirSeries:打点落在对应的 rpc × result 上,秒为单位,batch id 个数进计数直方图。
// 用增量断言,不依赖其它用例是否打过点。
func TestObserveLoginReadsLandOnTheirSeries(t *testing.T) {
	PrimeLoginReads()
	homeBefore := histogramCount(t, "data_service_home_zone_read_seconds", HomeZoneRPCHome, HomeZoneReadResultNotFound)
	dirBefore := histogramCount(t, "data_service_account_directory_rpc_seconds", AccountDirectoryRPCLookup, AccountDirectoryResultRejected)
	batchBefore := unlabeledHistogram(t, "data_service_home_zone_batch_ids")

	ObserveHomeZoneRead(HomeZoneRPCHome, HomeZoneReadResultNotFound, 3*time.Millisecond)
	ObserveAccountDirectoryRPC(AccountDirectoryRPCLookup, AccountDirectoryResultRejected, 2*time.Millisecond)
	ObserveHomeZoneBatchIDs(5)

	if got := histogramCount(t, "data_service_home_zone_read_seconds", HomeZoneRPCHome, HomeZoneReadResultNotFound); got != homeBefore+1 {
		t.Fatalf("home_zone_read_seconds{rpc=home,result=not_found} count = %d, want %d", got, homeBefore+1)
	}
	if got := histogramCount(t, "data_service_account_directory_rpc_seconds", AccountDirectoryRPCLookup, AccountDirectoryResultRejected); got != dirBefore+1 {
		t.Fatalf("account_directory_rpc_seconds{rpc=lookup,result=rejected} count = %d, want %d", got, dirBefore+1)
	}
	batchAfter := unlabeledHistogram(t, "data_service_home_zone_batch_ids")
	if batchAfter.count != batchBefore.count+1 || batchAfter.sum-batchBefore.sum != 5 {
		t.Fatalf("home_zone_batch_ids count/sum moved by %d/%v, want 1/5",
			batchAfter.count-batchBefore.count, batchAfter.sum-batchBefore.sum)
	}
}

// TestLoginReadNamesMatchStressSummarize 读 stress_summarize.ps1 的 $homeZoneReadResults / $accountDirectoryRpcs,
// 与本包取值逐项同序比对:两边不一致时脚本整列静默变成 n/a,只有这里能先红(同 go/db TestRedisStepNamesMatchStressSummarize)。
func TestLoginReadNamesMatchStressSummarize(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, tc := range []struct {
		variable string
		want     []string
	}{
		{"homeZoneReadResults", homeZoneReadResults},
		{"accountDirectoryRpcs", accountDirectoryRPCs},
	} {
		block := regexp.MustCompile(`(?s)\$` + tc.variable + `\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
		if block == nil {
			t.Fatalf("$%s list not found in %s", tc.variable, path)
		}
		var got []string
		for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
			got = append(got, string(m[1]))
		}
		if len(got) != len(tc.want) {
			t.Fatalf("stress_summarize.ps1 $%s = %v, want %v", tc.variable, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("stress_summarize.ps1 $%s[%d] = %q, want %q", tc.variable, i, got[i], tc.want[i])
			}
		}
	}
}

// histogramCount 取 family{rpc,result} 这条直方图序列的样本数;序列不存在时报错。
func histogramCount(t *testing.T, family, rpc, result string) uint64 {
	t.Helper()
	for _, m := range gatherFamily(t, family) {
		if labelKey(m, "rpc", "result") == rpc+"/"+result {
			return m.GetHistogram().GetSampleCount()
		}
	}
	t.Fatalf("%s{rpc=%q,result=%q} not found", family, rpc, result)
	return 0
}

type histogramSample struct {
	count uint64
	sum   float64
}

// unlabeledHistogram 取无 label 直方图唯一一条序列的样本数与和。
func unlabeledHistogram(t *testing.T, family string) histogramSample {
	t.Helper()
	m := gatherFamily(t, family)
	if len(m) != 1 {
		t.Fatalf("%s has %d series, want 1", family, len(m))
	}
	h := m[0].GetHistogram()
	return histogramSample{count: h.GetSampleCount(), sum: h.GetSampleSum()}
}
