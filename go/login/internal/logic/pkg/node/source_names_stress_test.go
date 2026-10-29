package node

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestGateDirectorySourcesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $gateDirectorySources,
// 与 login_gate_directory_reads_total 的 source 取值逐项同序比对:两边不一致时 L2 表整列静默变成 0 或 n/a,
// 只有这里能先红(同 go/db 的 TestRedisStepNamesMatchStressSummarize)。
func TestGateDirectorySourcesMatchStressSummarize(t *testing.T) {
	want := []string{readSourceCache, readSourceUnsynced, readSourceStale}
	path := filepath.Join("..", "..", "..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$gateDirectorySources\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$gateDirectorySources list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $gateDirectorySources = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $gateDirectorySources[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
