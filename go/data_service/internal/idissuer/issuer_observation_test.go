package idissuer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// afterReadCounterStore 在第一次读完记录后插入另一副本的完整预留，不依赖调度时序或 sleep。
type afterReadCounterStore struct {
	CounterStore
	once      sync.Once
	afterRead func(context.Context) error
}

func (s *afterReadCounterStore) Read(ctx context.Context) (Record, error) {
	r, err := s.CounterStore.Read(ctx)
	if err != nil {
		return Record{}, err
	}
	s.once.Do(func() { err = s.afterRead(ctx) })
	return r, err
}

func TestReserve_ConcurrentCommitDoesNotReportLocalRollback(t *testing.T) {
	h := newHarness(t)
	other := h.production(1)
	t.Cleanup(func() { _ = other.Close() })
	obs := newObserver()
	var otherLo, otherHi uint64
	iss := h.prodIssuer(1, func(o *Options) {
		o.OnReserve = obs.record
		o.Secondary = &afterReadCounterStore{
			CounterStore: o.Secondary,
			afterRead: func(ctx context.Context) error {
				var err error
				otherLo, otherHi, err = other.Allocate(ctx, "player", 100)
				return err
			},
		}
	})
	t.Cleanup(func() { _ = iss.Close() })

	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	a := &allocated{t: t}
	a.add(otherLo, otherHi)
	a.add(lo, hi)
	if obs.count(ReserveConflict) != 1 {
		t.Fatalf("插入的并发预留必须使旧读版本冲突一次，实际为 %d", obs.count(ReserveConflict))
	}
	if obs.count(ReserveLocalBehind) != 0 {
		t.Fatalf("正常并发提交不能上报本地双回档，实际为 %d", obs.count(ReserveLocalBehind))
	}
}

// afterReadRegistry 固定登记处旧读与其他副本完整提交之间的交错。
type afterReadRegistry struct {
	Registry
	once      sync.Once
	afterRead func(context.Context) error
}

func (r *afterReadRegistry) Read(ctx context.Context, region uint8) (ClaimRecord, error) {
	claim, err := r.Registry.Read(ctx, region)
	if err != nil {
		return ClaimRecord{}, err
	}
	r.once.Do(func() { err = r.afterRead(ctx) })
	return claim, err
}

func TestReserve_ConcurrentCommitsDoNotReportRegistryRollback(t *testing.T) {
	h := newHarness(t)
	first, second := h.production(1), h.prodIssuer(1, nil)
	t.Cleanup(func() { _ = first.Close() })
	t.Cleanup(func() { _ = second.Close() })
	obs := newObserver()
	var ranges [2][2]uint64
	iss := h.prodIssuer(1, func(o *Options) {
		o.OnReserve = obs.record
		o.Registry = &afterReadRegistry{
			Registry: o.Registry,
			afterRead: func(ctx context.Context) error {
				for n, other := range []*Issuer{first, second} {
					lo, hi, err := other.Allocate(ctx, "player", 100)
					if err != nil {
						return err
					}
					ranges[n] = [2]uint64{lo, hi}
				}
				return nil
			},
		}
	})
	t.Cleanup(func() { _ = iss.Close() })

	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	a := &allocated{t: t}
	for _, r := range ranges {
		a.add(r[0], r[1])
	}
	a.add(lo, hi)
	if obs.count(ReserveConflict) != 1 {
		t.Fatalf("插入的并发预留必须使旧读版本冲突一次，实际为 %d", obs.count(ReserveConflict))
	}
	if obs.count(ReserveRegistryBehind) != 0 {
		t.Fatalf("正常并发提交不能上报登记处落后，实际为 %d", obs.count(ReserveRegistryBehind))
	}
}

func TestReserve_RollbackIsReportedBeforeWriteFailure(t *testing.T) {
	for _, target := range []string{"local", "registry"} {
		t.Run(target, func(t *testing.T) {
			h := newHarness(t)
			first, second := h.production(1), h.prodIssuer(1, nil)
			t.Cleanup(func() { _ = first.Close() })
			t.Cleanup(func() { _ = second.Close() })
			a := &allocated{t: t}
			a.take(first, 100)
			p, s, g := h.primary.snapshot(), h.secondary.snapshot(), h.registry.snapshot()
			a.take(second, 100)
			want := ReserveRegistryBehind
			if target == "local" {
				h.primary.restore(p)
				h.secondary.restore(s)
				want = ReserveLocalBehind
			} else {
				h.registry.restore(g)
			}
			writeErr := errors.New("模拟主记录写失败")
			h.primary.setErrors(nil, writeErr)
			obs := newObserver()
			iss := h.prodIssuer(1, func(o *Options) { o.OnReserve = obs.record })
			t.Cleanup(func() { _ = iss.Close() })

			if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, writeErr) {
				t.Fatalf("预留应因注入的写错误失败，实际为 %v", err)
			}
			if obs.count(want) != 1 || obs.count(ReserveStoreError) != 1 || obs.count(ReserveOK) != 0 {
				t.Fatalf("写失败前仍须报告真实回退，并拒绝发号，实际回退=%d 存储错误=%d 成功=%d",
					obs.count(want), obs.count(ReserveStoreError), obs.count(ReserveOK))
			}
		})
	}
}

type failingRegistryRecheck struct {
	Registry
	reads atomic.Int32
	err   error
}

func (r *failingRegistryRecheck) Read(ctx context.Context, region uint8) (ClaimRecord, error) {
	if r.reads.Add(1) == 2 {
		return ClaimRecord{}, r.err
	}
	return r.Registry.Read(ctx, region)
}

func TestReserve_RegistryRecheckFailureDoesNotWriteOrIssue(t *testing.T) {
	h := newHarness(t)
	first, second := h.production(1), h.prodIssuer(1, nil)
	t.Cleanup(func() { _ = first.Close() })
	t.Cleanup(func() { _ = second.Close() })
	a := &allocated{t: t}
	a.take(first, 100)
	g := h.registry.snapshot()
	a.take(second, 100)
	h.registry.restore(g)
	primaryBefore, secondaryBefore, registryBefore := h.primary.get(), h.secondary.get(), h.registry.highWater(1)
	readErr := errors.New("模拟登记处复核读失败")
	obs := newObserver()
	iss := h.prodIssuer(1, func(o *Options) {
		o.Registry = &failingRegistryRecheck{Registry: o.Registry, err: readErr}
		o.OnReserve = obs.record
	})
	t.Cleanup(func() { _ = iss.Close() })

	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, readErr) {
		t.Fatalf("登记处复核读失败必须拒绝发号，实际为 %v", err)
	}
	if h.primary.get() != primaryBefore || h.secondary.get() != secondaryBefore || h.registry.highWater(1) != registryBefore {
		t.Fatal("登记处复核读失败后不能修改任何预留记录")
	}
	if obs.count(ReserveStoreError) != 1 || obs.count(ReserveOK) != 0 {
		t.Fatalf("必须报告存储错误且不预留成功，实际错误=%d 成功=%d", obs.count(ReserveStoreError), obs.count(ReserveOK))
	}
}
