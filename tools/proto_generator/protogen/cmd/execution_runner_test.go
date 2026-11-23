package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"protogen/logger"

	"go.uber.org/zap"
)

func TestRunParallelGroupWaitsForSubTasksAndRecordsAll(t *testing.T) {
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previousLogger })

	synctest.Test(t, func(t *testing.T) {
		runner := NewExecutionRunner()
		const taskCount = 12
		var completed atomic.Int32

		tasks := make([]NamedTask, 0, taskCount)
		for i := 0; i < taskCount; i++ {
			idx := i
			tasks = append(tasks, NamedTask{
				Name: fmt.Sprintf("task-%d", idx),
				Run: func(wg *sync.WaitGroup) {
					wg.Add(1)
					go func() {
						defer wg.Done()
						time.Sleep(15 * time.Millisecond)
						completed.Add(1)
					}()
				},
			})
		}

		runner.RunParallelGroup("ParallelGroup", tasks)

		if got := completed.Load(); got != taskCount {
			t.Fatalf("expected all %d async subtasks to finish before RunParallelGroup returned, got %d", taskCount, got)
		}
		if len(runner.taskRecords) != taskCount {
			t.Fatalf("expected %d task records without race drops, got %d", taskCount, len(runner.taskRecords))
		}
		for _, rec := range runner.taskRecords {
			if rec.Duration != 15*time.Millisecond {
				t.Errorf("task %s recorded %v, want 15ms", rec.Name, rec.Duration)
			}
		}
		if len(runner.groupRecords) != 1 || runner.groupRecords[0].Name != "Group: ParallelGroup" {
			t.Fatalf("unexpected groupRecords: %v", runner.groupRecords)
		}
		if runner.groupRecords[0].Duration != 15*time.Millisecond {
			t.Fatalf("group duration = %v, want 15ms", runner.groupRecords[0].Duration)
		}
	})
}

func TestRunSerialGroupWaitsForSubTasksInOrder(t *testing.T) {
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previousLogger })

	synctest.Test(t, func(t *testing.T) {
		runner := NewExecutionRunner()
		var order []string
		var mu sync.Mutex

		runner.RunSerialGroup("SerialGroup", []NamedTask{
			{
				Name: "first",
				Run: func(wg *sync.WaitGroup) {
					wg.Add(1)
					go func() {
						defer wg.Done()
						time.Sleep(10 * time.Millisecond)
						mu.Lock()
						order = append(order, "first")
						mu.Unlock()
					}()
				},
			},
			{
				Name: "second",
				Run: func(wg *sync.WaitGroup) {
					wg.Add(1)
					go func() {
						defer wg.Done()
						time.Sleep(5 * time.Millisecond)
						mu.Lock()
						order = append(order, "second")
						mu.Unlock()
					}()
				},
			},
		})

		if len(order) != 2 || order[0] != "first" || order[1] != "second" {
			t.Fatalf("serial tasks did not complete in order: %v", order)
		}
		if len(runner.taskRecords) != 2 {
			t.Fatalf("expected 2 task records, got %d", len(runner.taskRecords))
		}
		if runner.groupRecords[0].Duration != 15*time.Millisecond {
			t.Fatalf("serial group duration = %v, want 15ms", runner.groupRecords[0].Duration)
		}
	})
}
