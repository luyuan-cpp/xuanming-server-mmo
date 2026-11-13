package com.game.gateway.service;

import com.game.gateway.config.HomeZoneProperties;
import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.LeasedEtcdWriter;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.etcd.ZoneHealthRecord;
import com.game.gateway.repository.ZoneConfigRepository;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * 区服健康的发布方(2026-11-11,服务器缺口清单第 29 条;docs/design/gateway-zone-health-publish-20261111.md §4.2):
 * 本副本负责的每个 zone({@code gateway.home-zone-ids}),每拍读本 zone 的 gate、scene(按 zone 收窄的 list + watch)与目录行,
 * 算出一份汇总({@link ZoneHealthRecord}),只在显示会变时写进 etcd。读方是全部 zone 的全部网关({@link ZoneHealthProbeService})。
 *
 * <h2>什么时候写({@link #shouldPublish},纯函数)</h2>
 * <ul>
 *   <li>本进程在当前租约上还没写过 → 写(租约换了,旧记录已随旧租约消失);</li>
 *   <li>状态或分母变了 → 立即写;</li>
 *   <li>在线人数与已写的相差不到分母的 {@value #ONLINE_PUBLISH_STEP_PERCENT}%(向上取整,至少 1)→ 不写;</li>
 *   <li>相差够了且负载档变了 → 立即写;相差够了、档没变 → 距上次写满 {@link #ONLINE_MIN_PUBLISH_INTERVAL_MS} 毫秒才写。
 *       分母为 0(没有上限)时在线人数不影响显示,只随别的变化一起写。</li>
 * </ul>
 * 稳态几乎不写,写入量与 gate 每 10 秒一次的心跳脱钩;读方收到的事件上界是每条记录每 30 秒一次。
 *
 * <h2>失败</h2>
 * 「不用写」也算核实:已写的记录仍在阈值内描述这个 zone。一条记录连续 {@code zone-probe.status-ttl-ms} 没被核实
 * (读本 zone 失败、坏 NodeInfo、读目录失败、写失败),就把它删掉:在 TTL 内读方照旧看到上一份(last-known-good),
 * 过了 TTL 读方看不到这条、当作不知道。续租失败时本拍什么都不写,记录随旧租约到期消失。
 *
 * <p>只由调度器调用,同一时刻只有一个线程在跑,按 zone 的状态不跨线程共享。用固定频率调度,每拍先续租:租约 TTL 至少是周期的
 * 两倍(构造时校验)。续租靠这个循环自己,所以它不能被别的定时任务拖住:application.yaml 把调度线程数
 * ({@code spring.task.scheduling.pool.size})设成任务数,每个任务各有线程可用(ScheduledTaskThreadsTest 钉住)。
 */
@Component
public class ZoneHealthPublisher {

    /** 在线人数漂移到分母的这个百分比才值得重写。 */
    static final int ONLINE_PUBLISH_STEP_PERCENT = 1;
    /** 只因在线人数漂移(负载档没变)而重写,同一条记录至少隔这么久(毫秒)。 */
    static final long ONLINE_MIN_PUBLISH_INTERVAL_MS = 30_000L;
    /** 一个 zone 连续失败达到该轮数后升级为 ERROR。 */
    private static final int FAILURE_ALERT_THRESHOLD = 3;

    private static final Logger log = LoggerFactory.getLogger(ZoneHealthPublisher.class);

    private final GateWatcher gateWatcher;
    private final ZoneConfigRepository zoneConfigRepo;
    private final LeasedEtcdWriter writer;
    private final long statusTtlMs;
    private final LongSupplier monotonicMs;
    /** 本副本负责的每个 zone 的发布状态,按配置顺序;只在调度线程上读写。 */
    private final Map<Long, ZoneState> zones = new LinkedHashMap<>();
    private final Counter written;
    private final Counter withdrawn;
    private final Counter failed;

    @Autowired
    public ZoneHealthPublisher(GateWatcher gateWatcher,
                               ZoneConfigRepository zoneConfigRepo,
                               LeasedEtcdWriter writer,
                               HomeZoneProperties homeZones,
                               ZoneProbeProperties probeProps,
                               MeterRegistry meterRegistry) {
        this(gateWatcher, zoneConfigRepo, writer, homeZones.getHomeZoneIds(), probeProps.getIntervalMs(),
                probeProps.getStatusTtlMs(), meterRegistry, () -> System.nanoTime() / 1_000_000L);
    }

    /** 包内可见:单测注入负责的 zone、周期、TTL 与单调时钟。 */
    ZoneHealthPublisher(GateWatcher gateWatcher,
                        ZoneConfigRepository zoneConfigRepo,
                        LeasedEtcdWriter writer,
                        List<Long> homeZoneIds,
                        long intervalMs,
                        long statusTtlMs,
                        MeterRegistry meterRegistry,
                        LongSupplier monotonicMs) {
        if (intervalMs <= 0 || statusTtlMs < 2 * intervalMs) {
            throw new IllegalStateException("zone-probe.status-ttl-ms (" + statusTtlMs + ") must be at least twice"
                    + " zone-probe.interval-ms (" + intervalMs + "): the zone health publisher renews its etcd lease once per interval");
        }
        this.gateWatcher = gateWatcher;
        this.zoneConfigRepo = zoneConfigRepo;
        this.writer = writer;
        this.statusTtlMs = statusTtlMs;
        this.monotonicMs = monotonicMs;
        for (Long zoneId : homeZoneIds) {
            zones.put(zoneId, new ZoneState(zoneId));
        }
        this.written = outcomeCounter(meterRegistry, "written");
        this.withdrawn = outcomeCounter(meterRegistry, "withdrawn");
        this.failed = outcomeCounter(meterRegistry, "failed");
        if (zones.isEmpty()) {
            log.info("Zone health publisher idle: gateway.home-zone-ids is empty, this replica only reads zone health");
        } else {
            log.info("Zone health publisher: instance={}, home zones={}", writer.instanceId(), zones.keySet());
        }
    }

    @Scheduled(fixedRateString = "${zone-probe.interval-ms:5000}")
    public void publish() {
        if (zones.isEmpty()) {
            return;
        }
        long generation;
        try {
            generation = writer.renewLease();
        } catch (RuntimeException e) {
            // 本拍写不了任何记录;已写的记录随旧租约到期消失,读方随后当作不知道。
            for (ZoneState zone : zones.values()) {
                zone.recordFailure("renew the etcd lease", e);
            }
            return;
        }
        long now = monotonicMs.getAsLong();
        for (ZoneState zone : zones.values()) {
            publishZone(zone, generation, now);
        }
    }

    private void publishZone(ZoneState zone, long generation, long now) {
        if (zone.leaseGeneration != generation) {
            // 租约换了:挂在旧租约上的记录已随它消失。
            zone.published = null;
            zone.leaseGeneration = generation;
        }
        ZoneHealthRecord next;
        try {
            next = summarize(zone.zoneId);
        } catch (RuntimeException e) {
            zone.recordFailure("read the zone's nodes or directory row", e);
            withdrawIfUnverified(zone, now);
            return;
        }
        if (zone.published != null && !shouldPublish(zone.published, next, now - zone.writtenAtMs)) {
            zone.verifiedAtMs = now;
            zone.consecutiveFailures = 0;
            return;
        }
        try {
            writer.put(ZoneHealthRecord.key(zone.zoneId, writer.instanceId()), next.encode());
        } catch (RuntimeException e) {
            zone.recordFailure("write its health record", e);
            withdrawIfUnverified(zone, now);
            return;
        }
        AutoZoneStatus before = zone.published == null ? null : zone.published.status();
        if (before != next.status()) {
            log.info("Zone {} health published: {} -> {} (online={}, capacity={})",
                    zone.zoneId, before == null ? "none" : before, next.status(), next.onlineCount(), next.capacity());
        }
        zone.published = next;
        zone.writtenAtMs = now;
        zone.verifiedAtMs = now;
        zone.consecutiveFailures = 0;
        written.increment();
    }

    /** 已写的记录超过 TTL 没被核实:删掉,读方不再拿它当这个 zone 的现状。删失败下一拍再试;连 etcd 都不通时记录随租约到期消失。 */
    private void withdrawIfUnverified(ZoneState zone, long now) {
        if (zone.published == null || now - zone.verifiedAtMs <= statusTtlMs) {
            return;
        }
        try {
            writer.delete(ZoneHealthRecord.key(zone.zoneId, writer.instanceId()));
        } catch (RuntimeException e) {
            log.warn("Zone {} health record is stale but could not be withdrawn: {}", zone.zoneId, e.getMessage());
            return;
        }
        log.warn("Zone {} health record withdrawn: not verified for {} ms", zone.zoneId, now - zone.verifiedAtMs);
        zone.published = null;
        withdrawn.increment();
    }

    /** 读本 zone 的 gate、scene 与目录行,算出汇总。目录里没有这一行时只按 gate 算分母。 */
    private ZoneHealthRecord summarize(long zoneId) {
        List<NodeInfoRecord> gates = gateWatcher.fetchZoneGateNodes(zoneId);
        List<NodeInfoRecord> scenes = gateWatcher.fetchZoneSceneNodes(zoneId);
        int directoryCapacity = zoneConfigRepo.findById(zoneId).map(ZoneConfig::getCapacity).orElse(0);
        return summarize(zoneId, gates, scenes, directoryCapacity);
    }

    /** 汇总的算法:状态与分母的口径都在 {@link ZoneHealthProbeService},与改动前的探测相同。 */
    static ZoneHealthRecord summarize(long zoneId, List<NodeInfoRecord> gates, List<NodeInfoRecord> scenes, int directoryCapacity) {
        long online = 0;
        for (NodeInfoRecord gate : gates) {
            online += gate.getPlayerCount();
        }
        return new ZoneHealthRecord(zoneId,
                ZoneHealthProbeService.evaluateHealth(gates, scenes),
                online,
                ZoneHealthProbeService.effectiveCapacity(directoryCapacity, gates));
    }

    /**
     * 已写出 {@code published} 之后,新算出的 {@code next} 值不值得重写(规则见类注释)。
     *
     * @param sinceLastWriteMs 距上次写这条记录的毫秒数
     */
    static boolean shouldPublish(ZoneHealthRecord published, ZoneHealthRecord next, long sinceLastWriteMs) {
        if (published.status() != next.status() || published.capacity() != next.capacity()) {
            return true;
        }
        long capacity = next.capacity();
        if (capacity <= 0) {
            return false;
        }
        long drift = Math.abs(next.onlineCount() - published.onlineCount());
        if (drift < onlineStep(capacity)) {
            return false;
        }
        if (ZoneHealthProbeService.calculateLoadLevel(published.onlineCount(), capacity)
                != ZoneHealthProbeService.calculateLoadLevel(next.onlineCount(), capacity)) {
            return true;
        }
        return sinceLastWriteMs >= ONLINE_MIN_PUBLISH_INTERVAL_MS;
    }

    /** 分母为 {@code capacity} 时,在线人数漂移多少才值得重写:分母的 1%,向上取整,至少 1。 */
    static long onlineStep(long capacity) {
        return Math.max(1L, (capacity * ONLINE_PUBLISH_STEP_PERCENT + 99) / 100);
    }

    private static Counter outcomeCounter(MeterRegistry registry, String outcome) {
        return Counter.builder("gateway.zone_health.publish")
                .description("Zone health records this gateway wrote, withdrew, or failed to publish")
                .tag("outcome", outcome)
                .register(registry);
    }

    /** 一个本副本负责的 zone 的发布状态。 */
    private final class ZoneState {
        final long zoneId;
        /** 下面 {@link #published} 写在哪一代租约上;0 = 还没在任何租约上写过。 */
        long leaseGeneration;
        /** 本进程在当前租约上写出的最后一份;null = etcd 里没有本进程为这个 zone 写的记录。 */
        ZoneHealthRecord published;
        long writtenAtMs;
        /** 最近一次确认 {@link #published} 仍描述这个 zone 的单调时刻。 */
        long verifiedAtMs;
        int consecutiveFailures;

        ZoneState(long zoneId) {
            this.zoneId = zoneId;
        }

        void recordFailure(String what, RuntimeException e) {
            consecutiveFailures++;
            failed.increment();
            if (consecutiveFailures >= FAILURE_ALERT_THRESHOLD) {
                log.error("Zone {} health publisher could not {} {} times in a row: {}",
                        zoneId, what, consecutiveFailures, e.getMessage(), e);
            } else {
                log.warn("Zone {} health publisher could not {} (attempt {}): {}", zoneId, what, consecutiveFailures, e.getMessage());
            }
        }
    }
}
