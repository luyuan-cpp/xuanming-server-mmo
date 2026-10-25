package dispatcher

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDeliveryOutcomesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $deliveryOutcomes,
// 与 deliveryOutcomes 逐项同序比对:两边不一致时 L3 表的列静默变成 n/a,只有这里能先红
// (同 go/db 的 TestTaskResultNotifyNamesMatchStressSummarize)。
func TestDeliveryOutcomesMatchStressSummarize(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$deliveryOutcomes\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$deliveryOutcomes list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(deliveryOutcomes) {
		t.Fatalf("stress_summarize.ps1 $deliveryOutcomes = %v, want %v", got, deliveryOutcomes)
	}
	for i := range got {
		if got[i] != deliveryOutcomes[i] {
			t.Fatalf("stress_summarize.ps1 $deliveryOutcomes[%d] = %q, want %q", i, got[i], deliveryOutcomes[i])
		}
	}
}
