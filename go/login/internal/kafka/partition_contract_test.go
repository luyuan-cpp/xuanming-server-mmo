package kafka

import (
	"context"
	"testing"

	db_proto "proto/db"
)

func TestPartitionsMatchContractRequiresExactContiguousSet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		partitions []int32
		expected   int
		want       bool
	}{
		{name: "exact", partitions: []int32{2, 0, 1}, expected: 3, want: true},
		{name: "expanded", partitions: []int32{0, 1, 2, 3}, expected: 3},
		{name: "missing", partitions: []int32{0, 2}, expected: 3},
		{name: "duplicate", partitions: []int32{0, 1, 1}, expected: 3},
		{name: "out of range", partitions: []int32{0, 1, 3}, expected: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := partitionsMatchContract(tc.partitions, tc.expected); got != tc.want {
				t.Fatalf("partitionsMatchContract(%v, %d)=%v want %v", tc.partitions, tc.expected, got, tc.want)
			}
		})
	}
}

func TestPartitionDriftIrreversiblyFencesDBTaskSends(t *testing.T) {
	p := &KeyOrderedKafkaProducer{topic: "db_task_zone_1", partitionCnt: 2}
	if err := p.verifyPartitionContract([]int32{0, 1, 2}); err == nil {
		t.Fatal("expanded broker partition set must be rejected")
	}
	if !p.routingFenced.Load() {
		t.Fatal("partition drift must fence the producer")
	}
	if err := p.verifyPartitionContract([]int32{0, 1}); err != nil {
		t.Fatalf("pure verifier should recognize the original set: %v", err)
	}
	err := p.SendTask(context.Background(), &db_proto.DBTask{TaskId: "must-not-send"}, "1")
	if err == nil {
		t.Fatal("a fenced producer must reject sends even if broker metadata later looks normal")
	}
}
