package svc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/breaker"
)

// data_service 按逻辑库建连熔断的单测(docs/design/data-service-breaker-per-logical-db.md §7)。
// go-zero 的具名熔断表是进程级的,每个用例用独立的虚构库名避免串扰。

func TestLogicalDBConnectGateIsolatesFailingDatabase(t *testing.T) {
	gate := newLogicalDBConnectGate()
	badDB := "test_breaker_isolated_bad_db"
	goodDB := "test_breaker_isolated_good_db"
	dialErr := errors.New("dial tcp: i/o timeout")

	opened := false
	for i := 0; i < 200; i++ {
		settle, err := gate(context.Background(), badDB)
		if err != nil {
			if !errors.Is(err, breaker.ErrServiceUnavailable) {
				t.Fatalf("open error must wrap breaker.ErrServiceUnavailable, got %v", err)
			}
			if !strings.Contains(err.Error(), badDB) {
				t.Fatalf("open error must name the logical database %q, got %q", badDB, err.Error())
			}
			opened = true
			break
		}
		settle(dialErr)
	}
	if !opened {
		t.Fatalf("expected breaker for %s to open after repeated connect failures", badDB)
	}

	// 坏库熔断打开期间,健康逻辑库的建连每次都要放行。
	for i := 0; i < 50; i++ {
		settle, err := gate(context.Background(), goodDB)
		if err != nil {
			t.Fatalf("healthy logical database %s was blocked at attempt %d: %v", goodDB, i, err)
		}
		settle(nil)
	}
}

// 调用方取消既不算失败也不算成功:取消夹在真实的建连失败之间时,熔断照样要打开。
// 把取消记成成功的话,3 次取消配 1 次失败会让「成功数 × 1.5」一直大于总数,坏库永远不被挡。
func TestLogicalDBConnectGateDoesNotCountCancellationAsSuccess(t *testing.T) {
	gate := newLogicalDBConnectGate()
	dbName := "test_breaker_canceled_between_failures_db"
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	dialErr := errors.New("dial tcp: i/o timeout")

	for round := 0; round < 200; round++ {
		for i := 0; i < 3; i++ {
			settle, err := gate(canceledCtx, dbName)
			if err != nil {
				return // 已经打开
			}
			settle(context.Canceled)
		}
		settle, err := gate(context.Background(), dbName)
		if err != nil {
			return // 已经打开
		}
		settle(dialErr)
	}
	t.Fatalf("breaker for %s never opened: caller cancellations must not be recorded as successes", dbName)
}

func TestLogicalDBConnectGateIgnoresCallerCancellation(t *testing.T) {
	gate := newLogicalDBConnectGate()
	dbName := "test_breaker_canceled_only_db"
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 200; i++ {
		settle, err := gate(canceledCtx, dbName)
		if err != nil {
			t.Fatalf("caller cancellation must never trip the breaker (attempt %d): %v", i, err)
		}
		settle(context.Canceled)
	}
}
