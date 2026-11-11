package com.game.gateway.service;

import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.dto.LoadLevel;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.etcd.NodeType;
import com.game.gateway.etcd.ZoneHealthRecord;
import com.game.gateway.etcd.ZoneHealthWatcher;
import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;

import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.function.LongSupplier;

/**
 * 区服健康的读方:每个 zone 的自动健康状态与负载档,给区服列表读(内存快照,读接口无 I/O)。
 *
 * <h2>数据从哪来(2026-11-11 起,服务器缺口清单第 29 条)</h2>
 * 每个 zone 的健康由本 zone 的网关副本自己算好,写成 etcd 里的一条汇总记录({@link ZoneHealthPublisher}、{@link ZoneHealthRecord});
 * 这里每 {@code zone-probe.interval-ms} 读一次全部汇总({@link ZoneHealthWatcher},list + watch,稳态只收变更),
 * 连同 login 的登记一起发布成一份快照。本类不再 watch 全服 gate / scene,也不再每轮读区服目录:原来每个网关副本都为全服
 * 算健康,每台 gate 每 10 秒一次的心跳要送到每一个网关副本,事件量随 zone 数平方增长
 * (docs/design/gateway-zone-health-publish-20261111.md)。
 *
 * <h2>语义(与改动前相同的部分)</h2>
 * <ul>
 *   <li>读失败(etcd 不可用、有坏记录)保留上一份快照;快照超过 {@code zone-probe.status-ttl-ms} 或从没成功过,一律 UNKNOWN,
 *       不下发负载档。绝不把「读不到」当「全挂了」。</li>
 *   <li>负载档与负载率的口径不变:在线 / 分母,分母见 {@link #effectiveCapacity}(由发布者算好写进记录)。</li>
 * </ul>
 *
 * <h2>没有汇总记录的 zone</h2>
 * 本 zone 的网关副本都不在时,它的记录随租约消失。这时看 login 的登记:
 * <ul>
 *   <li>login 也没有登记 → DOWN。整个 zone 不在(或从没部署),assign-gate 必然失败,列表显示维护;</li>
 *   <li>login 还在 → UNKNOWN。只是本 zone 的网关不在,gate 多半还在接客,按「探测面不可用不等于区服 DOWN」显示开放、不带负载档。
 *       这种 zone 的个数导出为 {@code gateway_zone_health_unpublished_zones},告警见 deploy/k8s/gateway-zone-health-alerts.yaml。</li>
 * </ul>
 */
@Service
public class ZoneHealthProbeService {

    private static final Logger log = LoggerFactory.getLogger(ZoneHealthProbeService.class);

    /** 连续失败达到该轮数后升级为 ERROR(默认探测间隔 5s → 约 15s 无新鲜数据)。 */
    private static final int STALE_ALERT_THRESHOLD = 3;

    /**
     * gate 人数到自报连接上限的这个百分比就算到水位,与 login 挑 gate、排队区容量封顶同一条线
     * (go/shared/gatepick/eligibility.go 的 FullPercent)。ZoneHealthProbeServiceTest 从 Go 源码核对两边相等。
     */
    static final int GATE_FULL_PERCENT = 90;

    private final ZoneHealthWatcher healthWatcher;
    private final GateWatcher gateWatcher;
    private final ZoneProbeProperties probeProps;
    private final LongSupplier nowMs;
    /** 本服务创建的时刻;还没有成功快照时,快照年龄从这里算。 */
    private final long createdAtMs;

    /**
     * 全部结果和采集时间必须作为一份快照一次性发布。若逐项写共享 Map,读到一半失败会留下「部分新、部分旧」的混合状态,
     * 却仍被当成 last-known-good 对外提供。
     */
    private volatile ProbeSnapshot snapshot = ProbeSnapshot.empty();
    /** 连续读失败次数(读失败 ≠ 区服故障,只代表探测面不可用)。只在调度线程上读写。 */
    private int consecutiveProbeFailures = 0;

    @Autowired
    public ZoneHealthProbeService(ZoneHealthWatcher healthWatcher,
                                  GateWatcher gateWatcher,
                                  ZoneProbeProperties probeProps,
                                  MeterRegistry meterRegistry) {
        this(healthWatcher, gateWatcher, probeProps, meterRegistry, System::currentTimeMillis);
    }

    // 包级时钟注入点让冷启动和 TTL 测试不依赖真实时间。
    ZoneHealthProbeService(ZoneHealthWatcher healthWatcher,
                           GateWatcher gateWatcher,
                           ZoneProbeProperties probeProps,
                           MeterRegistry meterRegistry,
                           LongSupplier nowMs) {
        this.healthWatcher = healthWatcher;
        this.gateWatcher = gateWatcher;
        this.probeProps = probeProps;
        this.nowMs = nowMs;
        this.createdAtMs = nowMs.getAsLong();
        Gauge.builder("gateway.zone_health.unpublished_zones", this, s -> s.snapshot.unpublishedZones().size())
                .description("Zones with a registered login but no health record from their own gateway (shown as UNKNOWN)")
                .register(meterRegistry);
        Gauge.builder("gateway.zone_health.snapshot_age_seconds", this, ZoneHealthProbeService::snapshotAgeSeconds)
                .description("Seconds since this gateway last read the zone health records successfully")
                .register(meterRegistry);
    }

    @Scheduled(fixedDelayString = "${zone-probe.interval-ms:5000}")
    public void probe() {
        try {
            // 两个读在 etcd 查询失败或遇到坏记录时都抛 NodeDiscoveryException(而不是返回空),控制流直接进 catch,
            // 快照保持上一轮的 last-known-good。
            Map<Long, ZoneHealthRecord> records = healthWatcher.latestByZone();
            Set<Long> zonesWithLogin = zonesWithLogin(gateWatcher.fetchAllLoginNodes());

            Map<Long, AutoZoneStatus> statuses = new HashMap<>();
            Map<Long, LoadLevel> loadLevels = new HashMap<>();
            Map<Long, Long> onlineCounts = new HashMap<>();
            Map<Long, Long> capacities = new HashMap<>();
            for (ZoneHealthRecord record : records.values()) {
                statuses.put(record.zoneId(), record.status());
                loadLevels.put(record.zoneId(), calculateLoadLevel(record.onlineCount(), record.capacity()));
                onlineCounts.put(record.zoneId(), record.onlineCount());
                capacities.put(record.zoneId(), record.capacity());
            }
            Set<Long> unpublished = new TreeSet<>(zonesWithLogin);
            unpublished.removeAll(records.keySet());

            Set<Long> previouslyUnpublished = snapshot.unpublishedZones();
            snapshot = new ProbeSnapshot(
                    Map.copyOf(statuses),
                    Map.copyOf(loadLevels),
                    Map.copyOf(onlineCounts),
                    Map.copyOf(capacities),
                    Set.copyOf(zonesWithLogin),
                    Set.copyOf(unpublished),
                    nowMs.getAsLong());
            consecutiveProbeFailures = 0;
            if (!unpublished.equals(previouslyUnpublished)) {
                if (unpublished.isEmpty()) {
                    log.info("Every zone with a registered login has a health record again");
                } else {
                    log.warn("Zones with a registered login but no health record from their gateway, shown as UNKNOWN: {}", unpublished);
                }
            }
            log.debug("Zone probe completed: {} zone health records, {} zones with a login", records.size(), zonesWithLogin.size());
        } catch (Exception e) {
            consecutiveProbeFailures++;
            long staleForMs = snapshotAgeMs(snapshot);
            String serving = hasFreshSnapshot(snapshot) ? "last-known-good" : "UNKNOWN";
            if (consecutiveProbeFailures >= STALE_ALERT_THRESHOLD) {
                log.error("Zone health probe failed {} times in a row (snapshot age={} ms, serving {}): {}",
                        consecutiveProbeFailures, staleForMs, serving, e.getMessage(), e);
            } else {
                log.warn("Zone health probe failed (attempt {}, snapshot age={} ms, serving {}): {}",
                        consecutiveProbeFailures, staleForMs, serving, e.getMessage());
            }
        }
    }

    /**
     * 一个 zone 的自动健康状态,由本 zone 的发布者调用({@link ZoneHealthPublisher})。
     *
     * <p>无 gate 必须判 DOWN，不能判 DEGRADED。
     *
     * <p>gate 是玩家唯一的对外入口：AssignGate 要从 GateNodeService.rpc/ 里选出一个
     * gate 才能给客户端 ip/port/token。gates 为空时该区在玩家侧等价于完全不可登录，
     * 与「scene 挂了但 gate 还在」是两种性质完全不同的残缺态，不能合并成同一个
     * DEGRADED。
     *
     * <p>合并的后果不是「少报一档」而是彻底反向：ServerListService.resolveDisplayStatus
     * 只对 DOWN 做降级（→ MAINTENANCE），DEGRADED 没有任何分支、静默落到默认的
     * OPEN；与此同时 calculateLoadLevel 的分子只统计存活 gate 的 playerCount，
     * gates 为空 ⇒ totalPlayers=0 ⇒ ratio=0 ⇒ SMOOTH。于是一个 100% 连不上的区服
     * 对外显示成最诱人的「开放 · 流畅」，玩家点进去必然登录失败并反复重试，
     * 而运维侧拿不到任何自动降级信号（maintenanceMsg 也不会下发），只能人工去改
     * zone_config.manual_status。这同时违反「失败路径必须 fail-closed」。
     *
     * <p>现在 DEGRADED 只保留唯一含义：有 gate 但无 scene（能进得来，但进不了场景）。
     */
    static AutoZoneStatus evaluateHealth(List<NodeInfoRecord> gates, List<NodeInfoRecord> scenes) {
        if (gates.isEmpty()) {
            return AutoZoneStatus.DOWN;
        }
        if (scenes.isEmpty()) {
            return AutoZoneStatus.DEGRADED;
        }
        return AutoZoneStatus.HEALTHY;
    }

    /** 负载档:在线 / 分母 &lt; 0.5 流畅,&lt; 0.8 繁忙,否则爆满;分母 ≤ 0(没有上限)恒为流畅。发布者判断「跨档」也用它。 */
    static LoadLevel calculateLoadLevel(long totalPlayers, long capacity) {
        if (capacity <= 0) return LoadLevel.SMOOTH;
        double ratio = (double) totalPlayers / capacity;
        if (ratio < 0.5) return LoadLevel.SMOOTH;
        if (ratio < 0.8) return LoadLevel.BUSY;
        return LoadLevel.FULL;
    }

    /**
     * 负载档的分母:区服目录的 capacity 再按本区 gate 到水位一共能接的人数封顶(2026-11-10 起),与 login 排队的区容量
     * 同一口径(go/login/internal/logic/pkg/loginqueue/capacity.go 的 capByGates)。不封顶的话,gate 台数装不下目录容量时
     * 列表还显示「流畅 / 繁忙」,玩家点进去却在排队。2026-11-11 起由本 zone 的发布者算好、写进汇总记录。
     * <ul>
     *   <li>有 gate 没声明连接上限(maxConnections = 0:dev 下不设上限、旧版 gate)时不封顶;没有 gate 时区服本来就判 DOWN,
     *       这里也不封顶。</li>
     *   <li>目录 capacity ≤ 0(没配)时只看 gate;两边都没有上限时返回 0,负载档恒为流畅,同改动前。</li>
     *   <li>网关看不到 gate 的排空标记与停写判定(它们在 login 读的 Redis 标记与 etcd 版本里),这里数全部 gate,login 只数在接客的:
     *       排空、卡住期间列表会比排队乐观一点。排空是分钟级的计划内动作,停写的 gate 租约到期就从目录消失,接受这点偏差。</li>
     * </ul>
     */
    static long effectiveCapacity(int directoryCapacity, List<NodeInfoRecord> gates) {
        long directory = Math.max(directoryCapacity, 0);
        if (gates.isEmpty()) {
            return directory;
        }
        long gateCapacity = 0;
        for (NodeInfoRecord gate : gates) {
            if (gate.getMaxConnections() <= 0) {
                return directory;
            }
            gateCapacity += watermarkCapacity(gate.getMaxConnections());
        }
        return directory == 0 ? gateCapacity : Math.min(directory, gateCapacity);
    }

    /** 一台自报上限为 maxConnections 的 gate 到水位时的人数:ceil(maxConnections × 90 / 100),同 gatepick.WatermarkCapacity。 */
    static long watermarkCapacity(long maxConnections) {
        return (maxConnections * GATE_FULL_PERCENT + 99) / 100;
    }

    /** 有 login 登记的 zone。同 LoginNodeDiscovery,只认 login 类型的记录。 */
    private static Set<Long> zonesWithLogin(List<NodeInfoRecord> loginNodes) {
        Set<Long> zones = new HashSet<>();
        for (NodeInfoRecord node : loginNodes) {
            if (node.getNodeType() == NodeType.LOGIN_NODE_SERVICE) {
                zones.add(node.getZoneId());
            }
        }
        return zones;
    }

    public AutoZoneStatus getAutoStatus(long zoneId) {
        ProbeSnapshot current = snapshot;
        if (!hasFreshSnapshot(current)) {
            return AutoZoneStatus.UNKNOWN;
        }
        AutoZoneStatus status = current.statuses().get(zoneId);
        if (status != null) {
            return status;
        }
        // 没有汇总记录:login 也没有登记 = 整个 zone 不在,assign-gate 必然失败;login 还在 = 只是本 zone 的网关不在。
        return current.zonesWithLogin().contains(zoneId) ? AutoZoneStatus.UNKNOWN : AutoZoneStatus.DOWN;
    }

    public LoadLevel getLoadLevel(long zoneId) {
        ProbeSnapshot current = snapshot;
        if (!hasFreshSnapshot(current)) {
            return null;
        }
        return current.loadLevels().get(zoneId);
    }

    public long getOnlineCount(long zoneId) {
        ProbeSnapshot current = snapshot;
        if (!hasFreshSnapshot(current)) {
            return 0L;
        }
        return current.onlineCounts().getOrDefault(zoneId, 0L);
    }

    /**
     * 负载率 = 在线人数 / 负载档的分母(见 {@link #effectiveCapacity}),推荐区挑「最空的开放区」用。
     * 探测态不新鲜、或这个区没有汇总记录时返回 null;分母为 0(没有上限)时返回 0。
     */
    public Double getLoadRatio(long zoneId) {
        ProbeSnapshot current = snapshot;
        if (!hasFreshSnapshot(current)) {
            return null;
        }
        Long capacity = current.capacities().get(zoneId);
        if (capacity == null) {
            return null;
        }
        if (capacity <= 0) {
            return 0.0;
        }
        return (double) current.onlineCounts().getOrDefault(zoneId, 0L) / capacity;
    }

    private double snapshotAgeSeconds() {
        ProbeSnapshot current = snapshot;
        long since = current.capturedAtMs() == 0L ? createdAtMs : current.capturedAtMs();
        return Math.max(0L, nowMs.getAsLong() - since) / 1000.0;
    }

    private long snapshotAgeMs(ProbeSnapshot current) {
        long last = current.capturedAtMs();
        if (last == 0L) {
            return -1L;
        }
        return nowMs.getAsLong() - last;
    }

    private boolean hasFreshSnapshot(ProbeSnapshot current) {
        long age = snapshotAgeMs(current);
        long ttl = probeProps.getStatusTtlMs();
        return ttl > 0 && age >= 0 && age <= ttl;
    }

    /**
     * 一次成功读取的全部结果。{@code unpublishedZones} = 有 login 登记、却没有汇总记录的 zone(只给指标与日志用)。
     */
    private record ProbeSnapshot(Map<Long, AutoZoneStatus> statuses,
                                 Map<Long, LoadLevel> loadLevels,
                                 Map<Long, Long> onlineCounts,
                                 Map<Long, Long> capacities,
                                 Set<Long> zonesWithLogin,
                                 Set<Long> unpublishedZones,
                                 long capturedAtMs) {
        private static ProbeSnapshot empty() {
            return new ProbeSnapshot(Map.of(), Map.of(), Map.of(), Map.of(), Set.of(), Set.of(), 0L);
        }
    }
}
