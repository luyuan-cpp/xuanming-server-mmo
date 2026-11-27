// Package logwindow provides a lock-free, constant-memory log rate limiter for
// hot-path dependency degradation ("first error + once per window + streak count +
// single recovery log"), ported from xuanming-server/pkg/log/window.go.
//
// Deliberately unkeyed (zero per-player or per-entity buckets) so memory stays O(1)
// regardless of cardinality; per-entity metrics belong in bounded Prometheus counters.
package logwindow

import "sync/atomic"

// Window throttles repetitive dependency-degradation logs using only atomic integers.
// The zero value is ready to use; do not copy a Window after first use.
type Window struct {
	streak   atomic.Int64 // consecutive failures since last recovery
	extra    atomic.Int64 // optional secondary counter (e.g. dropped frames)
	loggedMs atomic.Int64 // UnixMilli timestamp of last emitted log
}

// Admit records one failure at nowMs and reports whether the caller should emit a log
// together with the current consecutive failure streak.
//   - The very first failure (streak == 1) is always admitted immediately.
//   - windowMs <= 0 disables throttling and admits every failure.
//   - Concurrent callers at a window boundary race on CompareAndSwap so only one logs.
func (w *Window) Admit(nowMs, windowMs int64) (shouldLog bool, streak int64) {
	n := w.streak.Add(1)
	if n == 1 {
		w.loggedMs.Store(nowMs)
		return true, n
	}
	if windowMs <= 0 {
		return true, n
	}
	last := w.loggedMs.Load()
	if nowMs-last >= windowMs && w.loggedMs.CompareAndSwap(last, nowMs) {
		return true, n
	}
	return false, n
}

// AddExtra adds n to the optional secondary counter and returns the new total.
func (w *Window) AddExtra(n int64) int64 { return w.extra.Add(n) }

// Recovered resets the failure streak on a successful call and returns the prior
// accumulated failure and extra counts. When failed > 0, the caller should emit a
// single recovery log.
func (w *Window) Recovered() (failed, extra int64) {
	failed = w.streak.Swap(0)
	if failed > 0 {
		extra = w.extra.Swap(0)
	}
	return failed, extra
}
