package main

import (
	"net"
	"net/http"
	"time"
)

// sharedHTTPClient is a process-wide HTTP client tuned for the robot's
// load-test traffic profile to the Java Gateway:
//
//   - 15k+ goroutines hitting the same host (Gateway) concurrently during
//     the open-server burst, each doing /api/login + /api/assign-gate +
//     occasional /api/queue-status polls.
//   - http.DefaultClient uses MaxIdleConnsPerHost=2 — under that load, the
//     transport hands out 2 reusable conns and creates+tears down a fresh
//     TCP socket for everyone else. Each torn-down conn lands in TIME_WAIT
//     and chews an ephemeral port for 60s. With 15k bots and ~3 calls per
//     login, we burn through the 64k ephemeral range in seconds and start
//     getting "cannot assign requested address" — the exact failure mode
//     post-mortemed in docs/stress/stress-test-2026-05-ephemeral-port.md.
//
// Tuning rationale:
//
//   - MaxIdleConnsPerHost = 4096 — comfortably above the per-process robot
//     count we expect to be in flight on the gateway at once. The Gateway
//     side keeps these warm via Tomcat's keep-alive.
//   - MaxIdleConns = 4096 — single-host workload, so equal to per-host.
//   - IdleConnTimeout = 90s — matches Tomcat's default keep-alive so we
//     don't get half-closed sockets racing with the next request.
//   - DisableCompression = true — gateway responses are small JSON; gzip
//     just adds CPU for no win at this size.
//
// We do NOT set a Client.Timeout here: every call site already wraps the
// request with context.WithTimeout, and a global timeout would silently
// override caller-specified deadlines.
var sharedHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   4096,
		MaxConnsPerHost:       0, // unlimited; backpressure comes from the gateway, not us
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false, // gateway is HTTP/1.1; HTTP/2 single-conn would serialize us
	},
}
