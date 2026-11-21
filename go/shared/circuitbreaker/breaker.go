// Package circuitbreaker provides a lightweight, concurrency-safe three-state
// (Closed / Open / HalfOpen) circuit breaker and per-endpoint manager for Go services.
//
// Designed for horizontally scalable stateless routers and RPC clients: each instance
// isolates failing downstream endpoints locally in memory with zero external coordination.
package circuitbreaker

import (
	"fmt"
	"sync"
	"time"
)

// State is the current circuit breaker state.
type State uint8

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// Breaker implements a standard three-state circuit breaker.
// Only downstream faults (timeouts, dial errors, 5xx/Unavailable) should call RecordFailure;
// caller-side cancellation or rate limits should call ReleaseProbe instead.
type Breaker struct {
	failureThreshold int
	openDuration     time.Duration
	now              func() time.Time

	mu            sync.Mutex
	failures      int
	openUntil     time.Time
	probeInFlight bool
}

// New creates a three-state Breaker. clock may be nil (defaults to time.Now).
func New(failureThreshold int, openDuration time.Duration, clock func() time.Time) *Breaker {
	if failureThreshold <= 0 {
		panic(fmt.Sprintf("circuitbreaker: failureThreshold must be > 0, got %d", failureThreshold))
	}
	if openDuration <= 0 {
		panic(fmt.Sprintf("circuitbreaker: openDuration must be > 0, got %s", openDuration))
	}
	if clock == nil {
		clock = time.Now
	}
	return &Breaker{
		failureThreshold: failureThreshold,
		openDuration:     openDuration,
		now:              clock,
	}
}

// State returns the observable circuit state at the current clock time.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probeInFlight {
		return StateHalfOpen
	}
	if b.failures >= b.failureThreshold {
		if b.now().Before(b.openUntil) {
			return StateOpen
		}
		return StateHalfOpen
	}
	return StateClosed
}

// Allow checks whether a request may proceed.
// When Open cooldown has elapsed, the first caller acquires the single HalfOpen probe slot
// and concurrent callers receive false until that probe completes.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probeInFlight {
		return false
	}
	if !b.openUntil.IsZero() && b.now().Before(b.openUntil) {
		return false
	}
	if b.failures >= b.failureThreshold {
		b.probeInFlight = true
	}
	return true
}

// RecordSuccess closes the circuit and resets consecutive failures.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.probeInFlight = false
	b.mu.Unlock()
}

// RecordFailure records a downstream fault. Once consecutive failures reach the threshold
// (or a HalfOpen probe fails), the circuit opens for openDuration.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	b.probeInFlight = false
	if b.failures < b.failureThreshold {
		b.failures++
	}
	if b.failures >= b.failureThreshold {
		b.openUntil = b.now().Add(b.openDuration)
	}
	b.mu.Unlock()
}

// ReleaseProbe releases a HalfOpen probe slot without changing failure count or open deadline
// (e.g., when the caller canceled its own context before downstream faulted).
func (b *Breaker) ReleaseProbe() {
	b.mu.Lock()
	b.probeInFlight = false
	b.mu.Unlock()
}

// Reset restores the breaker to a clean Closed state.
func (b *Breaker) Reset() { b.RecordSuccess() }

// EndpointSet maintains one independent Breaker per downstream endpoint ("ip:port").
// A nil *EndpointSet is valid and acts as a disabled no-op pass-through.
type EndpointSet struct {
	failureThreshold int
	openDuration     time.Duration
	now              func() time.Time

	mu       sync.RWMutex
	breakers map[string]*Breaker
}

// NewEndpointSet creates a per-endpoint breaker pool.
// If failureThreshold <= 0 or openDuration <= 0, returns nil (disabled pass-through).
func NewEndpointSet(failureThreshold int, openDuration time.Duration, clock func() time.Time) *EndpointSet {
	if failureThreshold <= 0 || openDuration <= 0 {
		return nil
	}
	if clock == nil {
		clock = time.Now
	}
	return &EndpointSet{
		failureThreshold: failureThreshold,
		openDuration:     openDuration,
		now:              clock,
		breakers:         make(map[string]*Breaker),
	}
}

func (s *EndpointSet) getOrCreate(endpoint string) *Breaker {
	s.mu.RLock()
	b := s.breakers[endpoint]
	s.mu.RUnlock()
	if b != nil {
		return b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b = s.breakers[endpoint]; b != nil {
		return b
	}
	b = New(s.failureThreshold, s.openDuration, s.now)
	s.breakers[endpoint] = b
	return b
}

func (s *EndpointSet) Allow(endpoint string) bool {
	if s == nil || endpoint == "" {
		return true
	}
	return s.getOrCreate(endpoint).Allow()
}

func (s *EndpointSet) RecordSuccess(endpoint string) {
	if s == nil || endpoint == "" {
		return
	}
	s.mu.RLock()
	b := s.breakers[endpoint]
	s.mu.RUnlock()
	if b != nil {
		b.RecordSuccess()
	}
}

func (s *EndpointSet) RecordFailure(endpoint string) {
	if s == nil || endpoint == "" {
		return
	}
	s.getOrCreate(endpoint).RecordFailure()
}

func (s *EndpointSet) ReleaseProbe(endpoint string) {
	if s == nil || endpoint == "" {
		return
	}
	s.mu.RLock()
	b := s.breakers[endpoint]
	s.mu.RUnlock()
	if b != nil {
		b.ReleaseProbe()
	}
}

// Remove deletes the breaker for an endpoint that left service discovery, preventing unbounded map growth.
func (s *EndpointSet) Remove(endpoint string) {
	if s == nil || endpoint == "" {
		return
	}
	s.mu.Lock()
	delete(s.breakers, endpoint)
	s.mu.Unlock()
}

func (s *EndpointSet) State(endpoint string) State {
	if s == nil || endpoint == "" {
		return StateClosed
	}
	s.mu.RLock()
	b := s.breakers[endpoint]
	s.mu.RUnlock()
	if b == nil {
		return StateClosed
	}
	return b.State()
}
