package com.game.gateway.service;

import com.game.gateway.config.ZoneProbeProperties;
import com.game.gateway.dto.AutoZoneStatus;
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

import java.util.List;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.assertEquals;
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
