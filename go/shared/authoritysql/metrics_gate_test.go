package authoritysql

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// 缺口清单第 20 条:闸门耗时按 (逻辑库, 步骤) 记,三个步骤各自一条序列,同一组标签再记一次不多出序列。
func TestObserveWriterGateRecordsEachPhase(t *testing.T) {
	const logicalDB = "metrics_gate_test_db"
	before := testutil.CollectAndCount(writerGateSeconds)
	for _, phase := range []string{gatePhaseBeforeBegin, gatePhaseEnter, gatePhaseRead} {
		observeWriterGate(logicalDB, phase, time.Now())
	}
	if got := testutil.CollectAndCount(writerGateSeconds) - before; got != 3 {
		t.Fatalf("三个步骤应各有一条序列,新增 %d 条", got)
	}
	observeWriterGate(logicalDB, gatePhaseEnter, time.Now().Add(-time.Millisecond))
	if got := testutil.CollectAndCount(writerGateSeconds) - before; got != 3 {
		t.Fatalf("同一组标签再记一次不应多出序列,新增 %d 条", got)
	}
}
