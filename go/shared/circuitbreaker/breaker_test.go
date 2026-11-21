package circuitbreaker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBreakerThreeStateTransitionsAndHalfOpenSingleProbe(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(3, 10*time.Second, func() time.Time { return now })

	require.Equal(t, StateClosed, b.State())
	require.True(t, b.Allow())
	b.RecordFailure()
	b.RecordFailure()
	require.Equal(t, StateClosed, b.State())

	// 3rd consecutive failure opens the circuit for 10s.
	b.RecordFailure()
	require.Equal(t, StateOpen, b.State())
	require.False(t, b.Allow())

	now = now.Add(9 * time.Second)
	require.Equal(t, StateOpen, b.State())
	require.False(t, b.Allow())

	// After 10s, enters HalfOpen and allows exactly 1 probe in flight.
	now = now.Add(1 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.Allow())
	require.False(t, b.Allow(), "concurrent caller must be blocked while HalfOpen probe is in flight")

	// Caller cancellation releases probe without re-opening or closing.
	b.ReleaseProbe()
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.Allow())

	// Probe failure immediately re-opens for another 10s.
	b.RecordFailure()
	require.Equal(t, StateOpen, b.State())
	require.False(t, b.Allow())

	// Next cooldown expiry + probe success closes the circuit.
	now = now.Add(10 * time.Second)
	require.True(t, b.Allow())
	b.RecordSuccess()
	require.Equal(t, StateClosed, b.State())
	require.True(t, b.Allow())
}

func TestEndpointSetIsolatesEndpointsAndCleansUpRemoved(t *testing.T) {
	now := time.Unix(2000, 0)
	set := NewEndpointSet(2, 5*time.Second, func() time.Time { return now })

	set.RecordFailure("10.0.0.1:9000")
	set.RecordFailure("10.0.0.1:9000")

	require.Equal(t, StateOpen, set.State("10.0.0.1:9000"))
	require.False(t, set.Allow("10.0.0.1:9000"))
	require.Equal(t, StateClosed, set.State("10.0.0.2:9000"))
	require.True(t, set.Allow("10.0.0.2:9000"))

	set.Remove("10.0.0.1:9000")
	require.Equal(t, StateClosed, set.State("10.0.0.1:9000"))
	require.True(t, set.Allow("10.0.0.1:9000"))

	// Disabled (nil) set always allows.
	var disabled *EndpointSet
	require.True(t, disabled.Allow("10.0.0.1:9000"))
	disabled.RecordFailure("10.0.0.1:9000")
	require.True(t, disabled.Allow("10.0.0.1:9000"))
}
