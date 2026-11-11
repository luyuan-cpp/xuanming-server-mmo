package com.game.gateway.service;

import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.dto.LoadLevel;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.data.redis.core.StringRedisTemplate;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.concurrent.atomic.AtomicLong;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class ZoneHealthProbeServiceTest {

    @Mock private GateWatcher gateWatcher;
    @Mock private ZoneConfigRepository zoneConfigRepo;
    @Mock private StringRedisTemplate redisTemplate;

    private final AtomicLong nowMs = new AtomicLong(1000L);
    private ZoneHealthProbeService service;

    @BeforeEach
    void setUp() {
        ZoneProbeProperties props = new ZoneProbeProperties();
        props.setStatusTtlMs(100L);
        service = new ZoneHealthProbeService(
                gateWatcher, zoneConfigRepo, redisTemplate, props, nowMs::get);
    }

    @Test
    void coldStartIsUnknownInsteadOfDown() {
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
    }

    @Test
    void successfulEmptySnapshotIsRealDownUntilTtlExpires() {
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone(1L)));
        when(gateWatcher.fetchAllGateNodes()).thenReturn(List.of());
        when(gateWatcher.fetchAllSceneNodes()).thenReturn(List.of());

        service.probe();
        assertEquals(AutoZoneStatus.DOWN, service.getAutoStatus(1L));

        nowMs.set(1101L);
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
        assertEquals(null, service.getLoadLevel(1L));
        assertEquals(0L, service.getOnlineCount(1L));
    }

    @Test
    void failedProbeServesLastKnownGoodOnlyWithinTtl() {
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone(1L)));
        when(gateWatcher.fetchAllGateNodes())
                .thenReturn(List.of(node(1L)))
                .thenThrow(new GateWatcher.NodeDiscoveryException("etcd unavailable", new RuntimeException("boom")));
        when(gateWatcher.fetchAllSceneNodes()).thenReturn(List.of(node(1L)));

        service.probe();
        assertEquals(AutoZoneStatus.HEALTHY, service.getAutoStatus(1L));

        nowMs.set(1050L);
        service.probe();
        assertEquals(AutoZoneStatus.HEALTHY, service.getAutoStatus(1L));

        nowMs.set(1101L);
        assertEquals(AutoZoneStatus.UNKNOWN, service.getAutoStatus(1L));
    }

    // 负载档的分母按 gate 到水位的承载封顶(2026-11-10):deploy/k8s/zones.json 的形状是每区 2 台、每台 20000,
    // 到 90% 水位一共 36000,装不下目录的 45000。在线 30000 时按目录算是 0.67(繁忙),排队却只剩 6000 个位置。
    @Test
    void loadLevelDenominatorCappedAtGateWatermark() {
        ZoneConfig zone = zone(1L);
        zone.setCapacity(45000);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(gateWatcher.fetchAllGateNodes()).thenReturn(List.of(gate(1L, 15000, 20000), gate(1L, 15000, 20000)));
        when(gateWatcher.fetchAllSceneNodes()).thenReturn(List.of(node(1L)));

        service.probe();

        assertEquals(LoadLevel.FULL, service.getLoadLevel(1L), "30000 / 36000 ≥ 0.8");
        assertEquals(30000.0 / 36000.0, service.getLoadRatio(1L), 1e-9);
    }

    // 有 gate 没声明连接上限(dev 下不设上限、旧版 gate)时不封顶,按目录容量算,同改动前。
    @Test
    void gateWithoutDeclaredLimitKeepsDirectoryCapacity() {
        ZoneConfig zone = zone(1L);
        zone.setCapacity(45000);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(gateWatcher.fetchAllGateNodes()).thenReturn(List.of(gate(1L, 15000, 20000), gate(1L, 15000, 0)));
        when(gateWatcher.fetchAllSceneNodes()).thenReturn(List.of(node(1L)));

        service.probe();

        assertEquals(LoadLevel.BUSY, service.getLoadLevel(1L), "30000 / 45000 < 0.8");
        assertEquals(30000.0 / 45000.0, service.getLoadRatio(1L), 1e-9);
    }

    // 目录没配容量(capacity ≤ 0)时只看 gate;以前这种区恒为流畅。
    @Test
    void unsetDirectoryCapacityUsesGatesOnly() {
        ZoneConfig zone = zone(1L);
        zone.setCapacity(0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(gateWatcher.fetchAllGateNodes()).thenReturn(List.of(gate(1L, 17000, 20000)));
        when(gateWatcher.fetchAllSceneNodes()).thenReturn(List.of(node(1L)));

        service.probe();

        assertEquals(LoadLevel.FULL, service.getLoadLevel(1L), "17000 / 18000 ≥ 0.8");
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
        NodeInfoRecord gate = node(zoneId);
        gate.setPlayerCount(playerCount);
        gate.setMaxConnections(maxConnections);
        return gate;
    }

    private static ZoneConfig zone(long zoneId) {
        ZoneConfig zone = new ZoneConfig();
        zone.setZoneId(zoneId);
        zone.setCapacity(5000);
        return zone;
    }

    private static NodeInfoRecord node(long zoneId) {
        NodeInfoRecord node = new NodeInfoRecord();
        node.setZoneId(zoneId);
        return node;
    }
}
