package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"protogen/logger"

	"go.uber.org/zap"
)

// WaitTimeRecord stores elapsed time by task or group name.
type WaitTimeRecord struct {
	Name     string
	Duration time.Duration
}

// NamedTask describes a named generation task that may spawn async work via WaitGroup.
type NamedTask struct {
	Name string
	Run  func(*sync.WaitGroup)
}

// ExecutionRunner centralizes timing, grouping and execution orchestration for pipeline stages.
type ExecutionRunner struct {
	mu           sync.Mutex
	groupRecords []WaitTimeRecord
	taskRecords  []WaitTimeRecord
}

func NewExecutionRunner() *ExecutionRunner {
	return &ExecutionRunner{}
}

func (r *ExecutionRunner) RunTask(name string, fn func()) {
	start := time.Now()
	fn()
	elapsed := time.Since(start)

	r.mu.Lock()
	r.taskRecords = append(r.taskRecords, WaitTimeRecord{Name: name, Duration: elapsed})
	r.mu.Unlock()

	logger.Global.Info("Function execution time",
		zap.String("funcName", name),
		zap.Duration("elapsed", elapsed),
	)
}

func (r *ExecutionRunner) RunTaskWithWG(name string, fn func(*sync.WaitGroup)) {
	r.RunTask(name, func() {
		var wg sync.WaitGroup
		fn(&wg)
		wg.Wait()
	})
}

func (r *ExecutionRunner) RunParallelGroup(groupName string, tasks []NamedTask) {
	var groupWg sync.WaitGroup
	start := time.Now()

	for _, task := range tasks {
		task := task
		groupWg.Add(1)
		go func() {
			defer groupWg.Done()
			r.RunTask(task.Name, func() {
				var taskWg sync.WaitGroup
				task.Run(&taskWg)
				taskWg.Wait()
			})
		}()
	}

	groupWg.Wait()
	r.recordGroup(groupName, start, "Group total execution time")
}

func (r *ExecutionRunner) RunSerialGroup(groupName string, tasks []NamedTask) {
	start := time.Now()

	for _, task := range tasks {
		task := task
		r.RunTask(task.Name, func() {
			var taskWg sync.WaitGroup
			task.Run(&taskWg)
			taskWg.Wait()
		})
	}

	r.recordGroup(groupName, start, "Serial Group total execution time")
}

func (r *ExecutionRunner) recordGroup(groupName string, start time.Time, logMessage string) {
	elapsed := time.Since(start)
	r.mu.Lock()
	r.groupRecords = append(r.groupRecords, WaitTimeRecord{
		Name:     "Group: " + groupName,
		Duration: elapsed,
	})
	r.mu.Unlock()

	logger.Global.Info(logMessage,
		zap.String("groupName", groupName),
		zap.Duration("elapsed", elapsed),
	)
}

func (r *ExecutionRunner) PrintStats() {
	fmt.Println("\n=== Group & Function Time Statistics ===")
	if len(r.groupRecords) > 0 {
		sortedGroups := make([]WaitTimeRecord, len(r.groupRecords))
		copy(sortedGroups, r.groupRecords)
		sort.Slice(sortedGroups, func(i, j int) bool {
			return sortedGroups[i].Duration > sortedGroups[j].Duration
		})

		for i, record := range sortedGroups {
			prefix := "  "
			if i == 0 {
				prefix = "\033[31m-> "
			}
			fmt.Printf("%s%d. %-60s %s\033[0m\n", prefix, i+1, record.Name, record.Duration)
		}
	}

	fmt.Println("\n=== Individual Function Time Statistics ===")
	if len(r.taskRecords) == 0 {
		return
	}

	sortedTasks := make([]WaitTimeRecord, len(r.taskRecords))
	copy(sortedTasks, r.taskRecords)
	sort.Slice(sortedTasks, func(i, j int) bool {
		return sortedTasks[i].Duration > sortedTasks[j].Duration
	})

	for i, record := range sortedTasks {
		prefix := "  "
		if i == 0 {
			prefix = "\033[33m-> "
		}
		fmt.Printf("%s%d. %-60s %s\033[0m\n", prefix, i+1, record.Name, record.Duration)
	}
}

func adaptSimpleTask(fn func()) func(*sync.WaitGroup) {
	return func(_ *sync.WaitGroup) {
		fn()
	}
}
