package com.game.gateway.service;

import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.dto.LoadLevel;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.repository.ZoneConfigRepository;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;

import java.util.*;
import java.util.function.LongSupplier;

/**
 * Periodically probes etcd + Redis to determine each zone's auto health status and load level.
 * Results are cached in-memory for fast reads by the server-list API.
 */
@Service
public class ZoneHealthProbeService {

    private static final Logger log = LoggerFactory.getLogger(ZoneHealthProbeService.class);

    private final GateWatcher gateWatcher;
    private final ZoneConfigRepository zoneConfigRepo;
    private final StringRedisTemplate redisTemplate;
    private final ZoneProbeProperties probeProps;
    private final LongSupplier nowMs;

    /**
     * 三类结果和采集时间必须作为一份快照一次性发布。若逐项写共享 Map，
     * Redis/etcd 在循环中途失败会留下“部分新、部分旧”的混合状态，却仍被
     * 当成 last-known-good 对外提供。
     */
    private volatile ProbeSnapshot snapshot = ProbeSnapshot.empty();

    @Autowired
    public ZoneHealthProbeService(GateWatcher gateWatcher,
                                  ZoneConfigRepository zoneConfigRepo,
                                  StringRedisTemplate redisTemplate,
                                  ZoneProbeProperties probeProps) {
        this(gateWatcher, zoneConfigRepo, redisTemplate, probeProps, System::currentTimeMillis);
    }

    // 包级时钟注入点让冷启动和 TTL 测试不依赖真实时间。
    ZoneHealthProbeService(GateWatcher gateWatcher,
                           ZoneConfigRepository zoneConfigRepo,
                           StringRedisTemplate redisTemplate,
                           ZoneProbeProperties probeProps,
                           LongSupplier nowMs) {
        this.gateWatcher = gateWatcher;
        this.zoneConfigRepo = zoneConfigRepo;
        this.redisTemplate = redisTemplate;
        this.probeProps = probeProps;
        this.nowMs = nowMs;
    }

    /** 连续探测失败次数(探测失败 ≠ 区服故障,只代表探测面不可用)。 */
    private int consecutiveProbeFailures = 0;
    /** 连续失败达到该轮数后升级为 ERROR(默认探测间隔 5s → 约 15s 无新鲜数据)。 */
    private static final int STALE_ALERT_THRESHOLD = 3;

    /**
     * gate 人数到自报连接上限的这个百分比就算到水位,与 login 挑 gate、排队区容量封顶同一条线
     * (go/shared/gatepick/eligibility.go 的 FullPercent)。ZoneHealthProbeServiceTest 从 Go 源码核对两边相等。
     */
    static final int GATE_FULL_PERCENT = 90;

    @Scheduled(fixedDelayString = "${zone-probe.interval-ms:5000}")
    public void probe() {
        try {
            List<ZoneConfig> zones = zoneConfigRepo.findAll();
            // fetchAll* 在 etcd 查询失败时抛 NodeDiscoveryException(而不是返回
            // 空列表),控制流直接进入 catch —— 三个缓存保持上一轮的
            // last-known-good 值。绝不能把「查不到」当「全挂了」写进缓存:
            // 那会让所有 zone 显示维护中,把一次 etcd 抖动放大成全服假故障。
            List<NodeInfoRecord> gateNodes = gateWatcher.fetchAllGateNodes();
            List<NodeInfoRecord> sceneNodes = gateWatcher.fetchAllSceneNodes();

            // Group by zone
            Map<Long, List<NodeInfoRecord>> gatesByZone = groupByZone(gateNodes);
            Map<Long, List<NodeInfoRecord>> scenesByZone = groupByZone(sceneNodes);

            Map<Long, AutoZoneStatus> nextStatuses = new HashMap<>();
            Map<Long, LoadLevel> nextLoadLevels = new HashMap<>();
            Map<Long, Long> nextOnlineCounts = new HashMap<>();
            Map<Long, Long> nextCapacities = new HashMap<>();

            for (ZoneConfig zone : zones) {
                long zoneId = zone.getZoneId();
                List<NodeInfoRecord> gates = gatesByZone.getOrDefault(zoneId, Collections.emptyList());
                List<NodeInfoRecord> scenes = scenesByZone.getOrDefault(zoneId, Collections.emptyList());

                // Evaluate health
                AutoZoneStatus autoStatus = evaluateHealth(zoneId, gates, scenes);
                nextStatuses.put(zoneId, autoStatus);

                // Calculate load
                long totalPlayers = gates.stream().mapToLong(NodeInfoRecord::getPlayerCount).sum();
                long capacity = effectiveCapacity(zone.getCapacity(), gates);
                nextOnlineCounts.put(zoneId, totalPlayers);
                nextCapacities.put(zoneId, capacity);
                nextLoadLevels.put(zoneId, calculateLoadLevel(totalPlayers, capacity));
            }

            snapshot = new ProbeSnapshot(
                    Map.copyOf(nextStatuses),
                    Map.copyOf(nextLoadLevels),
                    Map.copyOf(nextOnlineCounts),
                    Map.copyOf(nextCapacities),
                    nowMs.getAsLong());
            consecutiveProbeFailures = 0;
            log.debug("Zone probe completed: {} zones checked", zones.size());
        } catch (Exception e) {
            consecutiveProbeFailures++;
            long staleForMs = snapshotAgeMs();
            String serving = hasFreshSnapshot() ? "last-known-good" : "UNKNOWN";
            if (consecutiveProbeFailures >= STALE_ALERT_THRESHOLD) {
                log.error("Zone health probe failed {} times in a row (snapshot age={} ms, serving {}): {}",
                        consecutiveProbeFailures, staleForMs, serving, e.getMessage(), e);
            } else {
                log.warn("Zone health probe failed (attempt {}, snapshot age={} ms, serving {}): {}",
                        consecutiveProbeFailures, staleForMs, serving, e.getMessage());
            }
        }
    }

    private AutoZoneStatus evaluateHealth(long zoneId,
                                          List<NodeInfoRecord> gates,
                                          List<NodeInfoRecord> scenes) {
        boolean hasGate = !gates.isEmpty();
        boolean hasScene = !scenes.isEmpty();

        // 无 gate 必须判 DOWN，不能判 DEGRADED。
        //
        // gate 是玩家唯一的对外入口：AssignGate 要从 GateNodeService.rpc/ 里选出一个
        // gate 才能给客户端 ip/port/token。gates 为空时该区在玩家侧等价于完全不可登录，
        // 与「scene 挂了但 gate 还在」是两种性质完全不同的残缺态，不能合并成同一个
        // DEGRADED。
        //
        // 合并的后果不是「少报一档」而是彻底反向：ServerListService.resolveDisplayStatus
        // 只对 DOWN 做降级（→ MAINTENANCE），DEGRADED 没有任何分支、静默落到默认的
        // OPEN；与此同时 calculateLoadLevel 的分子只统计存活 gate 的 playerCount，
        // gates 为空 ⇒ totalPlayers=0 ⇒ ratio=0 ⇒ SMOOTH。于是一个 100% 连不上的区服
        // 对外显示成最诱人的「开放 · 流畅」，玩家点进去必然登录失败并反复重试，
        // 而运维侧拿不到任何自动降级信号（maintenanceMsg 也不会下发），只能人工去改
        // zone_config.manual_status。这同时违反「失败路径必须 fail-closed」。
        //
        // 现在 DEGRADED 只保留唯一含义：有 gate 但无 scene（能进得来，但进不了场景）。
        if (!hasGate) {
            return AutoZoneStatus.DOWN;
        }
        if (!hasScene) {
            return AutoZoneStatus.DEGRADED;
        }
        return AutoZoneStatus.HEALTHY;
    }

    private LoadLevel calculateLoadLevel(long totalPlayers, long capacity) {
        if (capacity <= 0) return LoadLevel.SMOOTH;
        double ratio = (double) totalPlayers / capacity;
        if (ratio < 0.5) return LoadLevel.SMOOTH;
        if (ratio < 0.8) return LoadLevel.BUSY;
        return LoadLevel.FULL;
    }

    /**
     * 负载档的分母:区服目录的 capacity 再按本区 gate 到水位一共能接的人数封顶(2026-11-10 起),与 login 排队的区容量
     * 同一口径(go/login/internal/logic/pkg/loginqueue/capacity.go 的 capByGates)。不封顶的话,gate 台数装不下目录容量时
     * 列表还显示「流畅 / 繁忙」,玩家点进去却在排队。
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

    private Map<Long, List<NodeInfoRecord>> groupByZone(List<NodeInfoRecord> nodes) {
        Map<Long, List<NodeInfoRecord>> map = new HashMap<>();
        for (NodeInfoRecord n : nodes) {
            map.computeIfAbsent(n.getZoneId(), k -> new ArrayList<>()).add(n);
        }
        return map;
    }

    public AutoZoneStatus getAutoStatus(long zoneId) {
        ProbeSnapshot current = snapshot;
        if (!hasFreshSnapshot(current)) {
            return AutoZoneStatus.UNKNOWN;
        }
        return current.statuses().getOrDefault(zoneId, AutoZoneStatus.UNKNOWN);
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
     * 探测态不新鲜、或这个区不在快照里(刚加进目录)时返回 null;分母为 0(没有上限)时返回 0。
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

    private long snapshotAgeMs() {
        return snapshotAgeMs(snapshot);
    }

    private long snapshotAgeMs(ProbeSnapshot current) {
        long last = current.capturedAtMs();
        if (last == 0L) {
            return -1L;
        }
        return nowMs.getAsLong() - last;
    }

    private boolean hasFreshSnapshot() {
        return hasFreshSnapshot(snapshot);
    }

    private boolean hasFreshSnapshot(ProbeSnapshot current) {
        long age = snapshotAgeMs(current);
        long ttl = probeProps.getStatusTtlMs();
        return ttl > 0 && age >= 0 && age <= ttl;
    }

    private record ProbeSnapshot(Map<Long, AutoZoneStatus> statuses,
                                 Map<Long, LoadLevel> loadLevels,
                                 Map<Long, Long> onlineCounts,
                                 Map<Long, Long> capacities,
                                 long capturedAtMs) {
        private static ProbeSnapshot empty() {
            return new ProbeSnapshot(Map.of(), Map.of(), Map.of(), Map.of(), 0L);
        }
    }
}
