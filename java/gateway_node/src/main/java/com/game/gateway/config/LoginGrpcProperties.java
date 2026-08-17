package com.game.gateway.config;

import org.springframework.boot.context.properties.ConfigurationProperties;

/**
 * Connection settings for the gRPC client to {@code go/login} (login.rpc).
 *
 * <p>{@code endpoint} is expected as {@code host:port}. We deliberately do not
 * use go-zero's etcd resolver here: jetcd is already on the classpath, but
 * piping go-zero's URI scheme into grpc-java requires extra glue. Instead we
 * resolve via the existing {@link com.game.gateway.etcd.GateWatcher}-style
 * pattern when needed; for now a direct endpoint list is the simplest reliable
 * path. Zone-bound RPCs require {@code zoneId=host:port}; bare endpoints only
 * participate in the genuinely zone-less RefreshToken round-robin pool.
 */
@ConfigurationProperties(prefix = "login.grpc")
public class LoginGrpcProperties {

    /** {@code zoneId=host:port[,zoneId=host:port...]}; bare entries are RefreshToken-only. */
    private String endpoints = "127.0.0.1:50000";

    /** Per-call timeout. */
    private long timeoutMs = 3000;

    /** Optional client-side retry on UNAVAILABLE/DEADLINE_EXCEEDED. 0 disables. */
    private int retry = 1;

    /**
     * etcd-based login.rpc auto-discovery (multi-node deployments). When on,
     * the per-zone channel pool follows {@code LoginNodeService.rpc/} etcd
     * registrations; the static {@link #endpoints} list stays as a bootstrap /
     * fallback for zones with no live etcd registration. Turning this off
     * restores the pure static-config behaviour.
     */
    private boolean discoveryEnabled = true;

    /** Discovery poll interval (ms). */
    private long discoveryIntervalMs = 5000;

    public String getEndpoints() { return endpoints; }
    public void setEndpoints(String endpoints) { this.endpoints = endpoints; }
    public long getTimeoutMs() { return timeoutMs; }
    public void setTimeoutMs(long timeoutMs) { this.timeoutMs = timeoutMs; }
    public int getRetry() { return retry; }
    public void setRetry(int retry) { this.retry = retry; }
    public boolean isDiscoveryEnabled() { return discoveryEnabled; }
    public void setDiscoveryEnabled(boolean discoveryEnabled) { this.discoveryEnabled = discoveryEnabled; }
    public long getDiscoveryIntervalMs() { return discoveryIntervalMs; }
    public void setDiscoveryIntervalMs(long discoveryIntervalMs) { this.discoveryIntervalMs = discoveryIntervalMs; }
}
