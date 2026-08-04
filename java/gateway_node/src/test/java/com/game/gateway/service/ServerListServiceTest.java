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
