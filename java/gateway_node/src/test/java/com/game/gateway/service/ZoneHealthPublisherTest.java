package com.game.gateway.service;

import com.game.gateway.dto.AutoZoneStatus;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.LeasedEtcdWriter;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.etcd.ZoneHealthRecord;
import com.game.gateway.repository.ZoneConfigRepository;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.clearInvocations;
import static org.mockito.Mockito.doReturn;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link ZoneHealthPublisher}:只发布本副本负责的 zone;汇总的状态与分母口径同原探测;只在显示会变时写
 * (状态、分母变了立即写;在线人数漂移满分母 1% 时跨档立即写,否则 30 秒一次);租约换了全部重写;
 * 读不到或写不进时 TTL 内保留已写记录,过了 TTL 撤回;续租失败本拍什么都不做。
 */
class ZoneHealthPublisherTest {

    private static final long INTERVAL_MS = 5_000L;
    private static final long TTL_MS = 15_000L;
    private static final String KEY_1 = ZoneHealthRecord.key(1, "pub-1");

    private GateWatcher gateWatcher;
    private ZoneConfigRepository repo;
    private LeasedEtcdWriter writer;
    private SimpleMeterRegistry registry;
    private final AtomicLong clock = new AtomicLong(100_000L);
    private ZoneHealthPublisher publisher;

    @BeforeEach
    void setUp() {
        gateWatcher = mock(GateWatcher.class);
        repo = mock(ZoneConfigRepository.class);
        writer = mock(LeasedEtcdWriter.class);
        registry = new SimpleMeterRegistry();
        when(writer.instanceId()).thenReturn("pub-1");
        when(writer.renewLease()).thenReturn(1L);
        publisher = publisherFor(List.of(1L));
    }

    private ZoneHealthPublisher publisherFor(List<Long> homeZones) {
        return new ZoneHealthPublisher(gateWatcher, repo, writer, homeZones, INTERVAL_MS, TTL_MS, registry, clock::get);
    }

    /**
     * zone 1 现在有这些 gate、scene,目录容量为 directoryCapacity(null = 目录里没有这一行)。用 doReturn 打桩:
     * 前面的用例可能已把同一方法打成 thenThrow,when(mock.call()) 会在重新打桩时先把异常抛出来。
     */
    private void zone1(List<NodeInfoRecord> gates, List<NodeInfoRecord> scenes, Integer directoryCapacity) {
        doReturn(gates).when(gateWatcher).fetchZoneGateNodes(1L);
        doReturn(scenes).when(gateWatcher).fetchZoneSceneNodes(1L);
        if (directoryCapacity == null) {
            doReturn(Optional.empty()).when(repo).findById(1L);
        } else {
            ZoneConfig row = new ZoneConfig();
            row.setZoneId(1L);
            row.setCapacity(directoryCapacity);
            doReturn(Optional.of(row)).when(repo).findById(1L);
        }
    }

    /** 一台在线 playerCount、自报上限 maxConnections 的 gate。 */
    private static NodeInfoRecord gate(long playerCount, long maxConnections) {
        NodeInfoRecord gate = new NodeInfoRecord();
        gate.setZoneId(1L);
        gate.setPlayerCount(playerCount);
        gate.setMaxConnections(maxConnections);
        return gate;
    }

    private static List<NodeInfoRecord> gates(long... playerCounts) {
        List<NodeInfoRecord> gates = new ArrayList<>();
        for (long count : playerCounts) {
            gates.add(gate(count, 20_000));
        }
        return gates;
    }

    private static List<NodeInfoRecord> oneScene() {
        NodeInfoRecord scene = new NodeInfoRecord();
        scene.setZoneId(1L);
        return List.of(scene);
    }

    private static String encoded(AutoZoneStatus status, long online, long capacity) {
        return new ZoneHealthRecord(1, status, online, capacity).encode();
    }

    private void tick(long advanceMs) {
        clock.addAndGet(advanceMs);
        publisher.publish();
    }

    private double count(String outcome) {
        return registry.get("gateway.zone_health.publish").tag("outcome", outcome).counter().count();
    }

    @Test
    void emptyHomeZones_publishNothing_andTakeNoLease() {
        ZoneHealthPublisher idle = publisherFor(List.of());

        idle.publish();

        verify(writer, never()).renewLease();
        verify(writer, never()).put(anyString(), anyString());
    }

    @Test
    void ttlMustCoverTwoIntervals() {
        assertThrows(IllegalStateException.class,
                () -> new ZoneHealthPublisher(gateWatcher, repo, writer, List.of(1L), 10_000, 15_000, registry, clock::get),
                "每拍续一次租:错过一拍不能就失租");
    }

    // 2 台 gate、每台上限 2 万:到 90% 水位一共 36000,比目录的 45000 小,分母取 36000(同 11-10 的口径)。
    @Test
    void firstTick_writesSummaryWithCappedDenominator() {
        zone1(gates(15_000, 15_000), oneScene(), 45_000);

        tick(0);

        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 30_000, 36_000));
        assertEquals(1.0, count("written"));
    }

    @Test
    void statusFollowsGatesAndScenes_downDegradedHealthy() {
        zone1(List.of(), oneScene(), 45_000);
        tick(0);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.DOWN, 0, 45_000));

        zone1(gates(10), List.of(), 45_000);
        tick(INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.DEGRADED, 10, 18_000));

        zone1(gates(10), oneScene(), 45_000);
        tick(INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 10, 18_000));
    }

    @Test
    void missingDirectoryRow_usesGateCapacityOnly() {
        zone1(gates(100), oneScene(), null);

        tick(0);

        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 100, 18_000));
    }

    @Test
    void unchangedZone_isNotRewritten() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);

        tick(INTERVAL_MS);
        tick(INTERVAL_MS);

        verify(writer, times(1)).put(anyString(), anyString());
    }

    // 分母 18000,1% = 180。漂移不到 180 不写;够了但档位没变,距上次写满 30 秒才写。
    @Test
    void onlineDrift_isThrottled() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);

        zone1(gates(1_179), oneScene(), 45_000);
        tick(40_000);
        verify(writer, times(1)).put(anyString(), anyString());

        zone1(gates(1_180), oneScene(), 45_000);
        tick(INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 1_180, 18_000));

        zone1(gates(1_500), oneScene(), 45_000);
        tick(INTERVAL_MS);
        verify(writer, never()).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 1_500, 18_000));

        tick(ZoneHealthPublisher.ONLINE_MIN_PUBLISH_INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 1_500, 18_000));
    }

    // 跨档(这里从流畅到繁忙,18000 的 50% = 9000)不等 30 秒,立即写。
    @Test
    void loadLevelChange_isPublishedImmediately() {
        zone1(gates(8_900), oneScene(), 45_000);
        tick(0);

        zone1(gates(9_100), oneScene(), 45_000);
        tick(INTERVAL_MS);

        verify(writer).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 9_100, 18_000));
    }

    @Test
    void statusOrCapacityChange_isPublishedImmediately() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);

        zone1(gates(1_000), List.of(), 45_000);
        tick(INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.DEGRADED, 1_000, 18_000));

        zone1(gates(1_000, 0), List.of(), 45_000);
        tick(INTERVAL_MS);
        verify(writer).put(KEY_1, encoded(AutoZoneStatus.DEGRADED, 1_000, 36_000));
    }

    @Test
    void newLease_rewritesUnchangedRecord() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);

        when(writer.renewLease()).thenReturn(2L);
        tick(INTERVAL_MS);

        verify(writer, times(2)).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 1_000, 18_000));
    }

    @Test
    void readFailure_keepsRecordWithinTtl_thenWithdrawsIt_thenRepublishes() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);
        when(gateWatcher.fetchZoneGateNodes(1L)).thenThrow(new GateWatcher.NodeDiscoveryException("etcd unavailable", null));

        tick(INTERVAL_MS);
        tick(INTERVAL_MS);
        tick(INTERVAL_MS);
        verify(writer, never()).delete(anyString());

        tick(1);
        verify(writer).delete(KEY_1);
        assertEquals(1.0, count("withdrawn"));
        assertEquals(4.0, count("failed"));

        tick(INTERVAL_MS);
        verify(writer, times(1)).delete(KEY_1);

        zone1(gates(1_000), oneScene(), 45_000);
        tick(INTERVAL_MS);
        verify(writer, times(2)).put(KEY_1, encoded(AutoZoneStatus.HEALTHY, 1_000, 18_000));
    }

    @Test
    void directoryReadFailure_countsAsUnverified() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);
        when(repo.findById(1L)).thenThrow(new RuntimeException("db down"));

        tick(TTL_MS + 1);

        verify(writer).delete(KEY_1);
    }

    @Test
    void writeFailure_isRetried_andStaleRecordWithdrawnAfterTtl() {
        zone1(gates(1_000), oneScene(), 45_000);
        tick(0);
        zone1(gates(1_000), List.of(), 45_000);
        doThrow(new LeasedEtcdWriter.EtcdWriteException("put failed", null)).when(writer).put(anyString(), anyString());

        tick(INTERVAL_MS);
        tick(INTERVAL_MS);
        verify(writer, times(3)).put(anyString(), anyString());
        verify(writer, never()).delete(anyString());

        tick(INTERVAL_MS + 1);
        verify(writer).delete(KEY_1);
    }

    @Test
    void leaseFailure_skipsTheTick() {
        when(writer.renewLease()).thenThrow(new LeasedEtcdWriter.EtcdWriteException("etcd down", null));
        zone1(gates(1_000), oneScene(), 45_000);

        tick(0);

        verify(gateWatcher, never()).fetchZoneGateNodes(anyLong());
        verify(writer, never()).put(anyString(), anyString());
        assertEquals(1.0, count("failed"));
    }

    @Test
    void onlyHomeZonesArePublished() {
        zone1(gates(1), oneScene(), 45_000);

        tick(0);

        verify(gateWatcher).fetchZoneGateNodes(1L);
        verify(gateWatcher, never()).fetchZoneGateNodes(2L);
        clearInvocations(gateWatcher);
    }

    @Test
    void shouldPublish_rules() {
        ZoneHealthRecord base = new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_000, 10_000);

        assertFalse(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_099, 10_000), 60_000),
                "漂移 99 < 1% × 10000");
        assertFalse(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_100, 10_000), 29_999),
                "够 1% 但档位没变,不到 30 秒");
        assertTrue(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_100, 10_000), 30_000));
        assertTrue(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 900, 10_000), 30_000),
                "往下漂移同样算");
        assertTrue(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.DOWN, 1_000, 10_000), 0));
        assertTrue(ZoneHealthPublisher.shouldPublish(base, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_000, 9_000), 0));

        ZoneHealthRecord unlimited = new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 1_000, 0);
        assertFalse(ZoneHealthPublisher.shouldPublish(unlimited, new ZoneHealthRecord(1, AutoZoneStatus.HEALTHY, 50_000, 0), 3_600_000),
                "分母为 0 时负载档恒为流畅,在线人数不影响显示");
    }

    @Test
    void onlineStep_isOnePercentRoundedUp_atLeastOne() {
        assertEquals(1L, ZoneHealthPublisher.onlineStep(1));
        assertEquals(1L, ZoneHealthPublisher.onlineStep(100));
        assertEquals(2L, ZoneHealthPublisher.onlineStep(101));
        assertEquals(360L, ZoneHealthPublisher.onlineStep(36_000));
    }
}
