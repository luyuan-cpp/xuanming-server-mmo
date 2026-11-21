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
