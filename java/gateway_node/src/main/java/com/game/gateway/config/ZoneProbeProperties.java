package com.game.gateway.config;

import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.context.annotation.Configuration;

/**
 * 区服健康的读写周期与有效期({@code zone-probe.*})。
 *
 * <p>2026-11-12 删掉 {@code gate-stale-threshold-seconds}:从没有代码读它,留着只会让人以为网关在判 gate 停写
 * (停写由 login 按 etcd 版本判,见 go/shared/gatepick)。
 */
@Configuration
@ConfigurationProperties(prefix = "zone-probe")
public class ZoneProbeProperties {

    /** 读方读一次汇总、发布方算一次本区健康并续一次租约的周期。 */
    private long intervalMs = 5000;
    /**
     * 最近一次完整成功快照允许对外提供的最大时长;同时是健康汇总发布者的 etcd 租约时长
     * ({@code etcd.LeasedEtcdWriter}),必须不小于 {@link #intervalMs} 的两倍({@code service.ZoneHealthPublisher} 启动时校验)。
     */
    private long statusTtlMs = 15000;

    public long getIntervalMs() { return intervalMs; }
    public void setIntervalMs(long intervalMs) { this.intervalMs = intervalMs; }

    public long getStatusTtlMs() { return statusTtlMs; }
    public void setStatusTtlMs(long statusTtlMs) { this.statusTtlMs = statusTtlMs; }
}
