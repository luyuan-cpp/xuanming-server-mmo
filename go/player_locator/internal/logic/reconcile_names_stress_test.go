package logic

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// stressSummarizeList 读 tools/scripts/stress_summarize.ps1 里 `$<name> = @( … )` 的字符串数组。
func stressSummarizeList(t *testing.T, name string) []string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$` + regexp.QuoteMeta(name) + `\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$%s list not found in %s", name, path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	return got
}

func assertSameList(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $%s = %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}

// TestReconcileNamesMatchStressSummarize 把 stress_summarize.ps1 PL1 表依赖的两组取值与本包常量逐项同序比对:
// 两边不一致时 PL1 的对应列整列静默变成 0 或 n/a,只有这里能先红(同 login 的 TestGateDirectorySourcesMatchStressSummarize)。
//   - $plSessionActions ↔ reconcileSessionActions(reconcile_sessions_total 的 action);
//   - $plIndexRoundResults ↔ reconcileRoundCombos 里 loop=index 的 result,按出现顺序。
func TestReconcileNamesMatchStressSummarize(t *testing.T) {
	assertSameList(t, "plSessionActions", stressSummarizeList(t, "plSessionActions"), reconcileSessionActions)

	var indexResults []string
	for _, combo := range reconcileRoundCombos {
		if combo[0] == reconcileLoopIndex {
			indexResults = append(indexResults, combo[1])
		}
	}
	assertSameList(t, "plIndexRoundResults", stressSummarizeList(t, "plIndexRoundResults"), indexResults)
}
