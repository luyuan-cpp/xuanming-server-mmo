package com.game.gateway.service;

import com.game.gateway.config.HomeZoneProperties;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.repository.ZoneConfigRepository;
import io.grpc.Status;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Optional;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

/** 本区角色上限守卫(2026-11-12,区服满员处理 §6):只在设了上限时问角色数,到了才比较并写关上开关,问不到不关。 */
class RoleCapacityGuardTest {

    private static final long ZONE = 101L;
    private static final LocalDateTime NOW = LocalDateTime.of(2026, 11, 12, 10, 0);

    private ZoneConfigRepository repo;
    private LoginRpcClient loginRpc;
    private SimpleMeterRegistry registry;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        loginRpc = mock(LoginRpcClient.class);
        registry = new SimpleMeterRegistry();
    }

    private RoleCapacityGuard guard(Long... homeZones) {
        HomeZoneProperties props = new HomeZoneProperties();
        props.setHomeZoneIds(List.of(homeZones));
        return new RoleCapacityGuard(repo, props, loginRpc, registry, () -> NOW, () -> 0L);
    }

    private void zoneRow(long zoneId, int roleCapacity, boolean createRoleClosed) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(zoneId);
        z.setName("zone-" + zoneId);
        z.setRoleCapacity(roleCapacity);
        z.setCreateRoleClosed(createRoleClosed);
        when(repo.findById(zoneId)).thenReturn(Optional.of(z));
    }

    private double count(String outcome) {
        return registry.get("gateway.role_capacity.check").tag("outcome", outcome).counter().count();
    }

    private double gauge(String name, long zoneId) {
        return registry.get(name).tag("zone", Long.toString(zoneId)).gauge().value();
    }

    @Test
    void zoneWithoutCapIsNeverCountedOrClosed() {
        zoneRow(ZONE, 0, false);

        guard(ZONE).check();

        assertEquals(1, count("no_cap"));
        verifyNoInteractions(loginRpc);
        verify(repo, never()).closeCreateRoleAtRoleCapacity(anyLong(), anyInt(), any());
    }

    @Test
    void belowCapLeavesTheSwitchAloneAndExportsTheReading() {
        zoneRow(ZONE, 30_000, false);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenReturn(29_999L);

        guard(ZONE).check();

        assertEquals(1, count("below"));
        verify(repo, never()).closeCreateRoleAtRoleCapacity(anyLong(), anyInt(), any());
        assertEquals(29_999, gauge("gateway.zone.role_count", ZONE));
        assertEquals(30_000, gauge("gateway.zone.role_capacity", ZONE));
    }

    @Test
    void reachingCapClosesWithTheCapThatWasRead() {
        zoneRow(ZONE, 30_000, false);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenReturn(30_000L);
        when(repo.closeCreateRoleAtRoleCapacity(ZONE, 30_000, NOW)).thenReturn(1);

        guard(ZONE).check();

        assertEquals(1, count("closed"));
        verify(repo).closeCreateRoleAtRoleCapacity(ZONE, 30_000, NOW);
    }

    // 判断之后运维调了上限(或已经关上),比较并写一行没改:不算关上,下一拍按新值重判。
    @Test
    void closeThatLostTheRaceToAnOperatorChangeIsCountedAsRaced() {
        zoneRow(ZONE, 30_000, false);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenReturn(31_000L);
        when(repo.closeCreateRoleAtRoleCapacity(ZONE, 30_000, NOW)).thenReturn(0);

        guard(ZONE).check();

        assertEquals(1, count("raced"));
        assertEquals(0, count("closed"));
    }

    @Test
    void alreadyClosedZoneIsCountedButNotWrittenAgain() {
        zoneRow(ZONE, 30_000, true);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenReturn(30_500L);

        guard(ZONE).check();

        assertEquals(1, count("already_closed"));
        verify(repo, never()).closeCreateRoleAtRoleCapacity(anyLong(), anyInt(), any());
        assertEquals(30_500, gauge("gateway.zone.role_count", ZONE));
    }

    // 问不到不等于满了:不关,只记这一区的 error;下一个区照常检查。
    @Test
    void unreachableCountNeverClosesAndDoesNotStopOtherZones() {
        zoneRow(ZONE, 30_000, false);
        zoneRow(102L, 10, false);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenThrow(Status.UNAVAILABLE.asRuntimeException());
        when(loginRpc.getZoneRoleCount(102)).thenReturn(10L);
        when(repo.closeCreateRoleAtRoleCapacity(102L, 10, NOW)).thenReturn(1);

        guard(ZONE, 102L).check();

        assertEquals(1, count("error"));
        assertEquals(1, count("closed"));
        verify(repo, never()).closeCreateRoleAtRoleCapacity(eq(ZONE), anyInt(), any());
        verify(repo).closeCreateRoleAtRoleCapacity(102L, 10, NOW);
    }

    // 运维把上限置 0 之后不再问,导出的上限回 0(看板不再画旧上限),角色数停在最后一次读数。
    @Test
    void removingTheCapStopsCountingAndZeroesTheExportedCapacity() {
        zoneRow(ZONE, 30_000, false);
        when(loginRpc.getZoneRoleCount((int) ZONE)).thenReturn(12_000L);
        RoleCapacityGuard guard = guard(ZONE);
        guard.check();

        zoneRow(ZONE, 0, false);
        guard.check();

        assertEquals(1, count("below"));
        assertEquals(1, count("no_cap"));
        verify(loginRpc, times(1)).getZoneRoleCount((int) ZONE);
        assertEquals(0, gauge("gateway.zone.role_capacity", ZONE));
        assertEquals(12_000, gauge("gateway.zone.role_count", ZONE));
    }

    @Test
    void zoneMissingFromDirectoryIsSkipped() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        guard(ZONE).check();

        assertEquals(1, count("zone_missing"));
        verifyNoInteractions(loginRpc);
    }

    // 计数在启动时全部预置(封闭集合),第一次出现的结局也能被 increase() 看到。
    @Test
    void everyOutcomeSeriesExistsBeforeTheFirstCheck() {
        guard(ZONE);
        for (RoleCapacityGuard.Outcome outcome : RoleCapacityGuard.Outcome.values()) {
            assertEquals(0, count(outcome.label), outcome.label);
        }
    }
}
