package scope

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestLevelNamesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $matchPoolLevels,与 Level.String()
// (match_pool_claim_total / match_pool_wait_seconds 的 level label)逐项同序比对:两边不一致时 G2 表整行静默变成 0。
func TestLevelNamesMatchStressSummarize(t *testing.T) {
	want := make([]string, 0, LevelCount)
	for level := LevelZone; level < LevelCount; level++ {
		want = append(want, level.String())
	}
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$matchPoolLevels\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$matchPoolLevels list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $matchPoolLevels = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $matchPoolLevels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
