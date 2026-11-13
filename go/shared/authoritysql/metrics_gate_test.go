package authoritysql

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// 缺口清单第 20 条:闸门耗时按 (逻辑库, 步骤) 记,每个步骤各自一条序列,同一组标签再记一次不多出序列。
// 2026-11-12 加了不在每笔事务上的两步:pin(新建连接)、revalidate(mysql 档复用连接)。
func TestObserveWriterGateRecordsEachPhase(t *testing.T) {
	const logicalDB = "metrics_gate_test_db"
	phases := []string{gatePhaseBeforeBegin, gatePhaseEnter, gatePhaseRead, gatePhasePin, gatePhaseRevalidate}
	before := testutil.CollectAndCount(writerGateSeconds)
	for _, phase := range phases {
		observeWriterGate(logicalDB, phase, time.Now())
	}
	if got := testutil.CollectAndCount(writerGateSeconds) - before; got != len(phases) {
		t.Fatalf("%d 个步骤应各有一条序列,新增 %d 条", len(phases), got)
	}
	observeWriterGate(logicalDB, gatePhaseEnter, time.Now().Add(-time.Millisecond))
	if got := testutil.CollectAndCount(writerGateSeconds) - before; got != len(phases) {
		t.Fatalf("同一组标签再记一次不应多出序列,新增 %d 条", got)
	}
}
