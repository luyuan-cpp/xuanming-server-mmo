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

            for (ZoneConfig zone : zones) {
                long zoneId = zone.getZoneId();
                List<NodeInfoRecord> gates = gatesByZone.getOrDefault(zoneId, Collections.emptyList());
                List<NodeInfoRecord> scenes = scenesByZone.getOrDefault(zoneId, Collections.emptyList());

                // Evaluate health
                AutoZoneStatus autoStatus = evaluateHealth(zoneId, gates, scenes);
                nextStatuses.put(zoneId, autoStatus);

                // Calculate load
                long totalPlayers = gates.stream().mapToLong(NodeInfoRecord::getPlayerCount).sum();
                nextOnlineCounts.put(zoneId, totalPlayers);
                nextLoadLevels.put(zoneId, calculateLoadLevel(totalPlayers, zone.getCapacity()));
            }

            snapshot = new ProbeSnapshot(
                    Map.copyOf(nextStatuses),
                    Map.copyOf(nextLoadLevels),
                    Map.copyOf(nextOnlineCounts),
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

    private LoadLevel calculateLoadLevel(long totalPlayers, int capacity) {
        if (capacity <= 0) return LoadLevel.SMOOTH;
        double ratio = (double) totalPlayers / capacity;
        if (ratio < 0.5) return LoadLevel.SMOOTH;
        if (ratio < 0.8) return LoadLevel.BUSY;
        return LoadLevel.FULL;
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
                                 long capturedAtMs) {
        private static ProbeSnapshot empty() {
            return new ProbeSnapshot(Map.of(), Map.of(), Map.of(), 0L);
        }
    }
}
