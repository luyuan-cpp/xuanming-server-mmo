package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestPoolClaimOutcomesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $matchPoolClaimOutcomes,
// 与 match_pool_claim_total / match_immediate_claim_total 的 outcome 取值逐项同序比对:两边不一致时
// G1 / G3 表的结局列整列静默变成 0 或 n/a,只有这里能先红(同 login 的 TestGateDirectorySourcesMatchStressSummarize)。
func TestPoolClaimOutcomesMatchStressSummarize(t *testing.T) {
	want := []string{PoolClaimCommitted, PoolClaimConflict, PoolClaimNoNode, PoolClaimNoSlot, PoolClaimIDError, PoolClaimPartyGone}
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$matchPoolClaimOutcomes\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$matchPoolClaimOutcomes list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $matchPoolClaimOutcomes = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $matchPoolClaimOutcomes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
