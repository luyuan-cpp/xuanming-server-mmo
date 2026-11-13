package poolowner

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestSegmentRolesMatchStressSummarize 读 tools/scripts/stress_summarize.ps1 的 $matchSegmentRoles，与撮合段角色（Roles 的
// String()，指标 match_pool_owned / match_pool_segment_claim_total 的 role 标签）逐项同序比对：两边不一致时 G4 表的那一行
// 整行静默变成 0，只有这里能先红（同 metrics 包的 TestPoolClaimOutcomesMatchStressSummarize）。
func TestSegmentRolesMatchStressSummarize(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$matchSegmentRoles\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$matchSegmentRoles list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(Roles) {
		t.Fatalf("stress_summarize.ps1 $matchSegmentRoles = %v, want %d roles", got, len(Roles))
	}
	for i, role := range Roles {
		if got[i] != role.String() {
			t.Fatalf("stress_summarize.ps1 $matchSegmentRoles[%d] = %q, want %q", i, got[i], role.String())
		}
	}
}
