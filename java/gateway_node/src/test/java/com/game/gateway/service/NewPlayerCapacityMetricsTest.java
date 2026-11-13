package com.game.gateway.service;

import com.game.gateway.dto.ServerListResponse;
import com.game.gateway.dto.ZoneDisplayStatus;
import com.game.gateway.dto.ZoneInfoDto;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.dao.QueryTimeoutException;

import java.util.List;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;

/** 新玩家还有没有区可进(2026-11-12,区服满员处理 §6):两个 gauge 与区服列表同源,读不到时是 NaN 而不是 0。 */
class NewPlayerCapacityMetricsTest {

    private ServerListService serverList;
    private SimpleMeterRegistry registry;

    @BeforeEach
    void setUp() {
        serverList = mock(ServerListService.class);
        registry = new SimpleMeterRegistry();
        new NewPlayerCapacityMetrics(serverList, registry);
    }

    private static ZoneInfoDto zone(long zoneId, ZoneDisplayStatus status, boolean createRoleClosed) {
        ZoneInfoDto dto = new ZoneInfoDto();
        dto.setZoneId(zoneId);
        dto.setStatus(status);
        dto.setCreateRoleClosed(createRoleClosed);
        return dto;
    }

    private double gauge(String name) {
        return registry.get(name).gauge().value();
    }

    // 维护、预告的区不算开放;开放但暂停创角的区算开放、不算能建角。
    @Test
    void countsOpenZonesAndThoseStillAcceptingNewCharacters() {
        when(serverList.getServerList()).thenReturn(new ServerListResponse(List.of(
                zone(1, ZoneDisplayStatus.OPEN, false),
                zone(2, ZoneDisplayStatus.OPEN, true),
                zone(3, ZoneDisplayStatus.MAINTENANCE, false),
                zone(4, ZoneDisplayStatus.PREVIEW, false))));

        assertEquals(2, gauge("gateway.zones.open"));
        assertEquals(1, gauge("gateway.zones.accepting_new_players"));
    }

    // 告警要抓的状态:有开放的区,却没有一个能建角。
    @Test
    void everyOpenZoneClosedForCreationReadsAsZeroAccepting() {
        when(serverList.getServerList()).thenReturn(new ServerListResponse(List.of(
                zone(1, ZoneDisplayStatus.OPEN, true),
                zone(2, ZoneDisplayStatus.OPEN, true))));

        assertEquals(2, gauge("gateway.zones.open"));
        assertEquals(0, gauge("gateway.zones.accepting_new_players"));
    }

    // 全服维护:没有开放的区,告警的前半个条件就不成立。
    @Test
    void fullMaintenanceReadsAsNoOpenZone() {
        when(serverList.getServerList()).thenReturn(new ServerListResponse(List.of(
                zone(1, ZoneDisplayStatus.MAINTENANCE, false),
                zone(2, ZoneDisplayStatus.CLOSED, false))));

        assertEquals(0, gauge("gateway.zones.open"));
        assertEquals(0, gauge("gateway.zones.accepting_new_players"));
    }

    // 列表读不到(查库失败)时是 NaN:读不到不等于一个都没有,告警里的「== 0」不成立。
    @Test
    void unreadableServerListReadsAsNaNNotZero() {
        when(serverList.getServerList()).thenThrow(new QueryTimeoutException("zone_config load still in flight"));

        assertTrue(Double.isNaN(gauge("gateway.zones.open")));
        assertTrue(Double.isNaN(gauge("gateway.zones.accepting_new_players")));
    }
}
