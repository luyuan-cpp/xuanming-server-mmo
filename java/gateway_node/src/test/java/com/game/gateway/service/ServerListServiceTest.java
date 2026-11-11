package com.game.gateway.service;

import com.game.gateway.dto.*;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import java.time.LocalDateTime;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class ServerListServiceTest {

    @Mock
    private ZoneConfigRepository zoneConfigRepo;

    @Mock
    private ZoneHealthProbeService probeService;

    private ServerListService serverListService;

    @BeforeEach
    void setUp() {
        serverListService = new ServerListService(zoneConfigRepo, probeService);
    }

    @Test
    void openZoneWithHealthyProbe_returnsOpen() {
        ZoneConfig zone = makeZone(1L, "zone-1", 0); // OPEN
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.HEALTHY);
        when(probeService.getLoadLevel(1L)).thenReturn(LoadLevel.SMOOTH);

        ServerListResponse resp = serverListService.getServerList();

        assertEquals(1, resp.getZones().size());
        ZoneInfoDto dto = resp.getZones().getFirst();
        assertEquals(ZoneDisplayStatus.OPEN, dto.getStatus());
        assertEquals(LoadLevel.SMOOTH, dto.getLoadLevel());
    }

    @Test
    void openZoneWithDownProbe_returnsMaintenance() {
        ZoneConfig zone = makeZone(1L, "zone-1", 0); // manual OPEN
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.DOWN);

        ServerListResponse resp = serverListService.getServerList();

        assertEquals(ZoneDisplayStatus.MAINTENANCE, resp.getZones().getFirst().getStatus());
    }

    @Test
    void openZoneWithUnknownProbe_preservesWireStatusWithoutStaleLoad() {
        ZoneConfig zone = makeZone(1L, "zone-1", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.UNKNOWN);
        when(probeService.getLoadLevel(1L)).thenReturn(LoadLevel.FULL);

        ZoneInfoDto dto = serverListService.getServerList().getZones().getFirst();

        assertEquals(ZoneDisplayStatus.OPEN, dto.getStatus(),
                "internal UNKNOWN must preserve the existing four-value wire contract");
        assertNull(dto.getLoadLevel(), "stale load must not be shown for UNKNOWN health");
    }

    @Test
    void manualMaintenanceOverridesHealthy() {
        ZoneConfig zone = makeZone(1L, "zone-1", 1); // manual MAINTENANCE
        zone.setMaintenanceMsg("Updating to v2.0");
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.HEALTHY);

        ServerListResponse resp = serverListService.getServerList();

        ZoneInfoDto dto = resp.getZones().getFirst();
        assertEquals(ZoneDisplayStatus.MAINTENANCE, dto.getStatus());
        assertEquals("Updating to v2.0", dto.getMaintenanceMsg());
        assertNull(dto.getLoadLevel()); // no load shown during maintenance
    }

    @Test
    void closedZoneAlwaysClosed() {
        ZoneConfig zone = makeZone(1L, "zone-1", 2); // CLOSED
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.HEALTHY);

        assertEquals(ZoneDisplayStatus.CLOSED, serverListService.getServerList()
                .getZones().getFirst().getStatus());
    }

    @Test
    void previewZoneShowsOpenTime() {
        ZoneConfig zone = makeZone(1L, "New Server", 3); // PREVIEW
        zone.setOpenTime(LocalDateTime.of(2026, 4, 20, 10, 0));
        when(zoneConfigRepo.findAll()).thenReturn(List.of(zone));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.DOWN);

        ZoneInfoDto dto = serverListService.getServerList().getZones().getFirst();
        assertEquals(ZoneDisplayStatus.PREVIEW, dto.getStatus());
        assertNotNull(dto.getOpenTime());
    }

    // 推荐区(2026-11-10):运维推荐的区爆满时不再推荐,改推负载率最低的开放区。
    @Test
    void fullRecommendedZoneHandsRecommendationToEmptiestOpenZone() {
        ZoneConfig full = makeZone(1L, "zone-1", 0);
        full.setRecommended(true);
        ZoneConfig busy = makeZone(2L, "zone-2", 0);
        ZoneConfig smooth = makeZone(3L, "zone-3", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(full, busy, smooth));
        stubOpen(1L, LoadLevel.FULL, 0.95);
        stubOpen(2L, LoadLevel.BUSY, 0.6);
        stubOpen(3L, LoadLevel.SMOOTH, 0.2);

        assertEquals(List.of(false, false, true), recommendedFlags(serverListService.getServerList()));
    }

    // 运维推荐的区没爆满就照留,不去比较别的区。
    @Test
    void recommendedZoneNotFullStaysRecommended() {
        ZoneConfig busy = makeZone(1L, "zone-1", 0);
        busy.setRecommended(true);
        ZoneConfig smooth = makeZone(2L, "zone-2", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(busy, smooth));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.HEALTHY);
        when(probeService.getLoadLevel(1L)).thenReturn(LoadLevel.BUSY);
        when(probeService.getAutoStatus(2L)).thenReturn(AutoZoneStatus.HEALTHY);
        when(probeService.getLoadLevel(2L)).thenReturn(LoadLevel.SMOOTH);

        assertEquals(List.of(true, false), recommendedFlags(serverListService.getServerList()));
    }

    // 进不去的区(预告、维护、关闭)不推荐:默认选中它就登不上。
    @Test
    void zoneThatCannotBeEnteredIsNeverRecommended() {
        ZoneConfig preview = makeZone(1L, "New Server", 3);
        preview.setRecommended(true);
        ZoneConfig open = makeZone(2L, "zone-2", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(preview, open));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.DOWN);
        stubOpen(2L, LoadLevel.SMOOTH, 0.1);

        assertEquals(List.of(false, true), recommendedFlags(serverListService.getServerList()));
    }

    // 探测态未知时判不出爆满,运维的推荐照留;也不凭空补推荐。
    @Test
    void unknownProbeKeepsManualRecommendationAndAddsNone() {
        ZoneConfig recommended = makeZone(1L, "zone-1", 0);
        recommended.setRecommended(true);
        ZoneConfig other = makeZone(2L, "zone-2", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(recommended, other));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.UNKNOWN);
        when(probeService.getAutoStatus(2L)).thenReturn(AutoZoneStatus.UNKNOWN);
        assertEquals(List.of(true, false), recommendedFlags(serverListService.getServerList()));

        recommended.setRecommended(false);
        assertEquals(List.of(false, false), recommendedFlags(serverListService.getServerList()));
    }

    // 开放区全都爆满时仍推荐最空的那个(排队照样能进);负载率并列取列表里靠前的。
    @Test
    void allOpenZonesFullStillRecommendsLeastFull_tiesGoToEarlierZone() {
        ZoneConfig a = makeZone(1L, "zone-1", 0);
        ZoneConfig b = makeZone(2L, "zone-2", 0);
        ZoneConfig c = makeZone(3L, "zone-3", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(a, b, c));
        stubOpen(1L, LoadLevel.FULL, 0.99);
        stubOpen(2L, LoadLevel.FULL, 0.85);
        stubOpen(3L, LoadLevel.FULL, 0.85);

        assertEquals(List.of(false, true, false), recommendedFlags(serverListService.getServerList()));
    }

    // 暂停创建新角色的区(2026-11-10):列表照常下发 create_role_closed,推荐区既不保留它,也不自动挑它。
    @Test
    void zoneWithRoleCreationPausedIsShownButNeverRecommended() {
        ZoneConfig paused = makeZone(1L, "zone-1", 0);
        paused.setRecommended(true);
        paused.setCreateRoleClosed(true);
        ZoneConfig busy = makeZone(2L, "zone-2", 0);
        when(zoneConfigRepo.findAll()).thenReturn(List.of(paused, busy));
        when(probeService.getAutoStatus(1L)).thenReturn(AutoZoneStatus.HEALTHY);
        when(probeService.getLoadLevel(1L)).thenReturn(LoadLevel.SMOOTH);
        stubOpen(2L, LoadLevel.BUSY, 0.6);

        ServerListResponse resp = serverListService.getServerList();

        assertEquals(List.of(true, false), resp.getZones().stream().map(ZoneInfoDto::isCreateRoleClosed).toList());
        assertEquals(List.of(false, true), recommendedFlags(resp),
                "the emptier zone is paused for new characters, so the busier open zone is recommended instead");
    }

    private void stubOpen(long zoneId, LoadLevel level, double loadRatio) {
        when(probeService.getAutoStatus(zoneId)).thenReturn(AutoZoneStatus.HEALTHY);
        when(probeService.getLoadLevel(zoneId)).thenReturn(level);
        when(probeService.getLoadRatio(zoneId)).thenReturn(loadRatio);
    }

    private static List<Boolean> recommendedFlags(ServerListResponse resp) {
        return resp.getZones().stream().map(ZoneInfoDto::isRecommended).toList();
    }

    private ZoneConfig makeZone(Long id, String name, int manualStatus) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(id);
        z.setName(name);
        z.setManualStatus(manualStatus);
        z.setCapacity(5000);
        z.setSortOrder(0);
        return z;
    }
}
