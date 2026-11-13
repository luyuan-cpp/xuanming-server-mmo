package kafka

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"data_service/internal/store"
)

// TestAuditStreamNamesMatchStressSummarize 钉住 stress_summarize.ps1 A1 表的 $dsAuditStreams(2026-11-12)。
// 这组名字同时是 kafka_consumer_*{consumer} 的消费者名与 audit_retention_*{table} 的表名,A1 按它把两组指标放在同一行;
// 任一边改名而脚本没跟着改,A1 的对应列整列静默变成 n/a,只有这里能先红。
func TestAuditStreamNamesMatchStressSummarize(t *testing.T) {
	want := []string{ConsumerTxLog, ConsumerSnapshot}
	tables := []string{store.AuditTableTransactionLog, store.AuditTablePlayerSnapshot}
	for i := range want {
		if want[i] != tables[i] {
			t.Fatalf("consumer %q and retention table %q differ; stress_summarize.ps1 A1 joins them on one row", want[i], tables[i])
		}
	}

	path := filepath.Join("..", "..", "..", "..", "tools", "scripts", "stress_summarize.ps1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)\$dsAuditStreams\s*=\s*@\(([^)]*)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("$dsAuditStreams list not found in %s", path)
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		got = append(got, string(m[1]))
	}
	if len(got) != len(want) {
		t.Fatalf("stress_summarize.ps1 $dsAuditStreams = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stress_summarize.ps1 $dsAuditStreams[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
