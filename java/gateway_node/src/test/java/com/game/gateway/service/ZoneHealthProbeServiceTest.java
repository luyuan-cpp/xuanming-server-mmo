package com.game.gateway.service;

import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.dto.LoadLevel;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.etcd.NodeType;
import com.game.gateway.etcd.ZoneHealthRecord;
import com.game.gateway.etcd.ZoneHealthWatcher;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

/**
 * 区服健康读方:数据来自各 zone 发布的汇总记录(2026-11-11,服务器缺口清单第 29 条)。语义与改动前相同:冷启动与过期都是 UNKNOWN、
 * 不下发负载档;读失败只在 TTL 内提供上一份快照;负载档与负载率按记录里的在线数与分母算。没有记录的 zone 看 login 登记:
 * 没有 login → DOWN(整个 zone 不在),有 login → UNKNOWN(只是它的网关不在)。分母与水位的静态口径(2026-11-10)原样保留。
 */
class ZoneHealthProbeServiceTest {

    private ZoneHealthWatcher healthWatcher;
    private GateWatcher gateWatcher;
    private SimpleMeterRegistry registry;
    private final AtomicLong nowMs = new AtomicLong(1000L);
    private ZoneHealthProbeService service;

    @BeforeEach
    void setUp() {
        healthWatcher = mock(ZoneHealthWatcher.class);
        gateWatcher = mock(GateWatcher.class);
        registry = new SimpleMeterRegistry();
        ZoneProbeProperties props = new ZoneProbeProperties();
        props.setStatusTtlMs(100L);
        service = new ZoneHealthProbeService(healthWatcher, gateWatcher, props, registry, nowMs::get);
        when(gateWatcher.fetchAllLoginNodes()).thenReturn(List.of());
    }

    private static ZoneHealthRecord record(long zoneId, AutoZoneStatus status, long online, long capacity) {
        return new ZoneHealthRecord(zoneId, status, online, capacity);
    }

    private static NodeInfoRecord login(long zoneId) {
        NodeInfoRecord node = new NodeInfoRecord();
        node.setZoneId(zoneId);
        node.setNodeType(NodeType.LOGIN_NODE_SERVICE);
        return node;
    }

    private double gauge(String name) {
        return registry.get(name).gauge().value();
    }

    @Test
    void coldStartIsUnknownInsteadOfDown() {
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
        assertNull(service.getLoadLevel(1L));
        assertNull(service.getLoadRatio(1L));
    }

    @Test
    void publishedDownIsRealDownUntilTtlExpires() {
        when(healthWatcher.latestByZone()).thenReturn(Map.of(1L, record(1, AutoZoneStatus.DOWN, 0, 5000)));

        service.probe();
        assertEquals(AutoZoneStatus.DOWN, service.getAutoStatus(1L));

        nowMs.set(1101L);
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
        assertNull(service.getLoadLevel(1L));
        assertEquals(0L, service.getOnlineCount(1L));
    }

    @Test
    void failedReadServesLastKnownGoodOnlyWithinTtl() {
        when(healthWatcher.latestByZone())
                .thenReturn(Map.of(1L, record(1, AutoZoneStatus.HEALTHY, 10, 5000)))
                .thenThrow(new GateWatcher.NodeDiscoveryException("etcd unavailable", new RuntimeException("boom")));

        service.probe();
        assertEquals(AutoZoneStatus.HEALTHY, service.getAutoStatus(1L));

        nowMs.set(1050L);
        service.probe();
        assertEquals(AutoZoneStatus.HEALTHY, service.getAutoStatus(1L));

        nowMs.set(1101L);
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
    }

    // login 登记读失败同样算整次读失败,不能拿半份结果把没有记录的 zone 判成 DOWN。
    @Test
    void loginReadFailureAlsoKeepsLastKnownGood() {
        when(healthWatcher.latestByZone()).thenReturn(Map.of());
        when(gateWatcher.fetchAllLoginNodes())
                .thenReturn(List.of(login(1)))
                .thenThrow(new GateWatcher.NodeDiscoveryException("etcd unavailable", null));

        service.probe();
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));

        nowMs.set(1050L);
        service.probe();
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L), "上一份快照里 zone 1 有 login,仍是 UNKNOWN,不是 DOWN");
    }

    // 分母由发布者算好写进记录(2026-11-10 的封顶口径):30000 / 36000 ≥ 0.8 → 爆满。
    @Test
    void loadLevelAndRatioComeFromTheRecord() {
        when(healthWatcher.latestByZone()).thenReturn(Map.of(
                1L, record(1, AutoZoneStatus.HEALTHY, 30_000, 36_000),
                2L, record(2, AutoZoneStatus.HEALTHY, 30_000, 45_000),
                3L, record(3, AutoZoneStatus.HEALTHY, 30_000, 0)));

        service.probe();

        assertEquals(LoadLevel.FULL, service.getLoadLevel(1L), "30000 / 36000 ≥ 0.8");
        assertEquals(30000.0 / 36000.0, service.getLoadRatio(1L), 1e-9);
        assertEquals(LoadLevel.BUSY, service.getLoadLevel(2L), "30000 / 45000 < 0.8");
        assertEquals(LoadLevel.SMOOTH, service.getLoadLevel(3L), "没有上限恒为流畅");
        assertEquals(0.0, service.getLoadRatio(3L), 1e-9);
        assertEquals(30_000L, service.getOnlineCount(1L));
    }

    // 没有汇总记录:login 也没有 → 整个 zone 不在,DOWN(列表显示维护);login 还在 → 只是它的网关不在,UNKNOWN。
    @Test
    void zoneWithoutRecord_isDownOnlyWhenItsLoginIsGoneToo() {
        when(healthWatcher.latestByZone()).thenReturn(Map.of(1L, record(1, AutoZoneStatus.HEALTHY, 1, 100)));
        when(gateWatcher.fetchAllLoginNodes()).thenReturn(List.of(login(1), login(2)));

        service.probe();

        assertEquals(AutoZoneStatus.HEALTHY, service.getAutoStatus(1L));
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(2L), "zone 2 的 login 还在,只是网关没发布");
        assertEquals(AutoZoneStatus.DOWN, service.getAutoStatus(3L), "zone 3 连 login 都没有:assign-gate 必然失败");
        assertNull(service.getLoadLevel(2L));
        assertNull(service.getLoadLevel(3L));
        assertNull(service.getLoadRatio(3L));
    }

    @Test
    void unpublishedZonesAndSnapshotAgeAreExported() {
        assertEquals(0.0, gauge("gateway.zone_health.unpublished_zones"));
        nowMs.set(4000L);
        assertEquals(3.0, gauge("gateway.zone_health.snapshot_age_seconds"), 1e-9, "从没成功过:从服务创建起算");

        when(healthWatcher.latestByZone()).thenReturn(Map.of(1L, record(1, AutoZoneStatus.HEALTHY, 1, 100)));
        NodeInfoRecord notALogin = login(5);
        notALogin.setNodeType(NodeType.GATE_NODE_SERVICE);
        when(gateWatcher.fetchAllLoginNodes()).thenReturn(List.of(login(1), login(2), login(2), login(3), notALogin));
        service.probe();

        assertEquals(2.0, gauge("gateway.zone_health.unpublished_zones"), "zone 2、3 有 login、没有记录;非 login 类型的记录不算");
        assertEquals(0.0, gauge("gateway.zone_health.snapshot_age_seconds"), 1e-9);
        nowMs.set(6500L);
        assertEquals(2.5, gauge("gateway.zone_health.snapshot_age_seconds"), 1e-9);
    }

    @Test
    void evaluateHealthDistinguishesNoGateFromNoScene() {
        NodeInfoRecord node = new NodeInfoRecord();
        assertEquals(AutoZoneStatus.DOWN, ZoneHealthProbeService.evaluateHealth(List.of(), List.of(node)), "没有 gate 就是不可登录");
        assertEquals(AutoZoneStatus.DEGRADED, ZoneHealthProbeService.evaluateHealth(List.of(node), List.of()));
        assertEquals(AutoZoneStatus.HEALTHY, ZoneHealthProbeService.evaluateHealth(List.of(node), List.of(node)));
    }

    // 负载档的分母按 gate 到水位的承载封顶(2026-11-10):deploy/k8s/zones.json 的形状是每区 2 台、每台 20000,
    // 到 90% 水位一共 36000,装不下目录的 45000。在线 30000 时按目录算是 0.67(繁忙),排队却只剩 6000 个位置。
    // 2026-11-11 起分母由发布者算(ZoneHealthPublisher.summarize),这里直接核对静态口径。
    @Test
    void loadLevelDenominatorCappedAtGateWatermark() {
        long capacity = ZoneHealthProbeService.effectiveCapacity(45000, List.of(gate(1L, 15000, 20000), gate(1L, 15000, 20000)));
        assertEquals(36000L, capacity);
        assertEquals(LoadLevel.FULL, ZoneHealthProbeService.calculateLoadLevel(30000, capacity), "30000 / 36000 ≥ 0.8");
    }

    // 有 gate 没声明连接上限(dev 下不设上限、旧版 gate)时不封顶,按目录容量算,同改动前。
    @Test
    void gateWithoutDeclaredLimitKeepsDirectoryCapacity() {
        long capacity = ZoneHealthProbeService.effectiveCapacity(45000, List.of(gate(1L, 15000, 20000), gate(1L, 15000, 0)));
        assertEquals(45000L, capacity);
        assertEquals(LoadLevel.BUSY, ZoneHealthProbeService.calculateLoadLevel(30000, capacity), "30000 / 45000 < 0.8");
    }

    // 目录没配容量(capacity ≤ 0)时只看 gate;以前这种区恒为流畅。
    @Test
    void unsetDirectoryCapacityUsesGatesOnly() {
        long capacity = ZoneHealthProbeService.effectiveCapacity(0, List.of(gate(1L, 17000, 20000)));
        assertEquals(LoadLevel.FULL, ZoneHealthProbeService.calculateLoadLevel(17000, capacity), "17000 / 18000 ≥ 0.8");
    }

    @Test
    void effectiveCapacityTakesSmallerOfDirectoryAndGates() {
        assertEquals(36000L, ZoneHealthProbeService.effectiveCapacity(45000, List.of(gate(1L, 0, 20000), gate(1L, 0, 20000))));
        assertEquals(5000L, ZoneHealthProbeService.effectiveCapacity(5000, List.of(gate(1L, 0, 20000))));
        assertEquals(45000L, ZoneHealthProbeService.effectiveCapacity(45000, List.of()), "no gate: zone is DOWN anyway");
        assertEquals(0L, ZoneHealthProbeService.effectiveCapacity(0, List.of(gate(1L, 0, 0))), "no limit anywhere");
    }

    // 到水位的人数恰是 login 判「满」(playerCount*100 >= max*90)第一次成立的人数,两边对「满」的口径靠这条对齐。
    @Test
    void watermarkCapacityIsFirstFullCount() {
        for (long max : new long[] {1, 9, 10, 11, 15, 100, 101, 20000, 131071, 4294967295L}) {
            long w = ZoneHealthProbeService.watermarkCapacity(max);
            assertTrue(w * 100 >= max * ZoneHealthProbeService.GATE_FULL_PERCENT, "watermark count is full, max=" + max);
            assertTrue((w - 1) * 100 < max * ZoneHealthProbeService.GATE_FULL_PERCENT, "one below is not full, max=" + max);
        }
        assertEquals(18000L, ZoneHealthProbeService.watermarkCapacity(20000));
    }

    // 水位常量与 Go 侧挑 gate、排队封顶用的 gatepick.FullPercent 必须相等,改一边就要改另一边。
    @Test
    void gateFullPercentMatchesGoGatepick() throws IOException {
        Path source = findRepoFile("go/shared/gatepick/eligibility.go");
        Matcher m = Pattern.compile("(?m)^const FullPercent = (\\d+)\\s*$").matcher(Files.readString(source));
        assertTrue(m.find(), "const FullPercent not found in " + source);
        assertEquals(Integer.parseInt(m.group(1)), ZoneHealthProbeService.GATE_FULL_PERCENT);
    }

    /** 从工作目录(surefire 下是模块目录 java/gateway_node)往上找仓库里的文件。 */
    private static Path findRepoFile(String relative) {
        for (Path dir = Path.of(System.getProperty("user.dir")).toAbsolutePath(); dir != null; dir = dir.getParent()) {
            Path candidate = dir.resolve(relative);
            if (Files.isRegularFile(candidate)) {
                return candidate;
            }
        }
        throw new AssertionError(relative + " not found above " + System.getProperty("user.dir"));
    }

    private static NodeInfoRecord gate(long zoneId, long playerCount, long maxConnections) {
        NodeInfoRecord gate = new NodeInfoRecord();
        gate.setZoneId(zoneId);
        gate.setPlayerCount(playerCount);
        gate.setMaxConnections(maxConnections);
        return gate;
    }
}
