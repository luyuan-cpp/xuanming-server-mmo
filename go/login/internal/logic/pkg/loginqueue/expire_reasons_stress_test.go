package loginqueue

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestExpireReasonsMatchStressSummarize 钉住 stress_summarize.ps1 Q1 表的 $loginQueueExpireReasons(2026-11-12):
// 它要与 login_queue_expired_total{reason} 的 ExpireReason* 常量逐项同序。新增一个原因而脚本没跟着加,
// Q1 就少一列、看不到这类过期;改名而脚本没跟着改,那一列整列静默变成 0。只有这里能先红。
func TestExpireReasonsMatchStressSummarize(t *testing.T) {
	want := []string{
		ExpireReasonBadSignature,
		ExpireReasonTTL,
		ExpireReasonMetaGone,
		ExpireReasonQueueDisabled,
		ExpireReasonAdmitWindowPassed,
		ExpireReasonQueueGone,
	}
	path := filepath.Join("..", "..", "..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$loginQueueExpireReasons\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$loginQueueExpireReasons list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $loginQueueExpireReasons = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $loginQueueExpireReasons[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
