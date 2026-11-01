package assetop

import (
	"context"
	"errors"
	"fmt"
	"testing"

	assetpb "proto/common/asset"
	"shared/authoritysql"
	"shared/drainproof"
)

// 排空只认 HOLD:Wait 回 ErrHold 就是打过 HOLD。
func relayHeld(t *testing.T, relay *drainproof.Session) bool {
	t.Helper()
	relay.CloseAdmission()
	err := relay.Wait(context.Background())
	if err != nil && !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("Wait: %v", err)
	}
	return errors.Is(err, drainproof.ErrHold)
}

func TestFrozenGateDoesNotHoldRelay(t *testing.T) {
	loopWith := func(t *testing.T, finalize error) (*Loop, *drainproof.Session) {
		t.Helper()
		store := newFakeStore(testOp(1))
		if finalize != nil {
			store.finErr = map[uint64]error{1: finalize}
		}
		applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
			return resultApplied(true), nil
		}}
		loop, _ := newTestLoop(t, store, applier, nil)
		loop.Ledger = sqlLedgerFixture(true, testOp(1))
		relay := drainproof.New()
		loop.Relay = relay
		return loop, relay
	}

	t.Run("终结撞上冻结的闸门:确定没写,行还是 PENDING,不算结局未知", func(t *testing.T) {
		loop, relay := loopWith(t, fmt.Errorf("finalize: %w", authoritysql.ErrWriterFrozen))
		if _, err := loop.ProcessOne(context.Background(), testOp(1)); !authoritysql.IsWriterFrozen(err) {
			t.Fatalf("错误链要认得出冻结: %v", err)
		}
		if relayHeld(t, relay) {
			t.Fatal("冻结不打 HOLD")
		}
	})

	t.Run("结局未知照样 HOLD", func(t *testing.T) {
		loop, relay := loopWith(t, errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit")))
		if _, err := loop.ProcessOne(context.Background(), testOp(1)); err == nil {
			t.Fatal("要报错")
		}
		if !relayHeld(t, relay) {
			t.Fatal("结局未知要 HOLD")
		}
	})

	t.Run("冻结和别的失败混在一起照样 HOLD", func(t *testing.T) {
		loop, relay := loopWith(t, errors.Join(authoritysql.ErrWriterFrozen, errors.New("disk full")))
		if _, err := loop.ProcessOne(context.Background(), testOp(1)); err == nil {
			t.Fatal("要报错")
		}
		if !relayHeld(t, relay) {
			t.Fatal("混了别的失败不能算确定没写")
		}
	})
}
