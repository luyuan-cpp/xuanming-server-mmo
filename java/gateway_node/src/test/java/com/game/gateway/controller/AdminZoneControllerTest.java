package com.game.gateway.controller;

import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.dao.CannotAcquireLockException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;

import java.sql.SQLTransactionRollbackException;
import java.time.LocalDateTime;
import java.util.Optional;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.Mockito.*;

/**
 * AdminZoneController 的契约测试(死锁审计 #18 同类项的控制器侧)。
 *
 * <p>只验控制器自己的承诺:create 走 upsert + 回读且只对 1213 做有上限的整事务重试、缺键 400;update /
 * setMaintenance / setOpen 走按主键的单条 UPDATE、存在性由回读判定(不看行数)、绝不插入;delete 按删除行数回
 * 204 / 404;时间戳取自注入的时钟。「ODKU 在真库上不成环」由 {@code ZoneConfigUpsertLockOrderMySqlTest} 验,
 * 语句能否在 Spring Data 上执行由 {@code ZoneConfigRepositoryWiringTest} 验,这里替代不了。
 *
 * <p>事务用 {@link TransactionOperations#withoutTransaction()} 直通:这里不验事务边界。
 */
class AdminZoneControllerTest {

    private static final long ZONE = 7L;
    private static final LocalDateTime NOW = LocalDateTime.of(2026, 9, 21, 12, 0, 0);

    private ZoneConfigRepository repo;
    private AdminZoneController controller;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneConfigRepository.class);
        controller = new AdminZoneController(repo, TransactionOperations.withoutTransaction(), () -> NOW);
    }

    @Test
    void createUpsertsWholeRowAndReturnsStoredRow() {
        ZoneConfig body = zone(ZONE, "z");
        ZoneConfig stored = zone(ZONE, "z");
        when(repo.findById(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.create(body);

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody(), "返回库里回读的那一行,而不是回显请求体");
        verify(repo).upsert(body, NOW);
        verify(repo).findById(ZONE);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void createRejectsMissingZoneIdOrNameWithoutTouchingDb() {
        assertEquals(HttpStatus.BAD_REQUEST, controller.create(zone(null, "z")).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST, controller.create(zone(ZONE, null)).getStatusCode());
        verifyNoInteractions(repo);
    }

    @Test
    void createRetriesDeadlockVictimAsWholeUnitThenSucceeds() {
        ZoneConfig body = zone(ZONE, "z");
        when(repo.upsert(body, NOW)).thenThrow(deadlock()).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.create(body).getStatusCode());
        verify(repo, times(2)).upsert(body, NOW);
        verify(repo, times(1)).findById(ZONE);
    }

    @Test
    void createGivesUpAfterBoundedAttemptsAndRethrows() {
        CannotAcquireLockException dl = deadlock();
        when(repo.upsert(any(), any())).thenThrow(dl);

        CannotAcquireLockException thrown =
                assertThrows(CannotAcquireLockException.class, () -> controller.create(zone(ZONE, "z")));

        assertSame(dl, thrown, "用尽重试后原样抛出,不包装、不吞");
        verify(repo, times(AdminWhitelistController.DEADLOCK_MAX_ATTEMPTS)).upsert(any(), any());
        verify(repo, never()).findById(anyLong());
    }

    @Test
    void createFailsLoudlyWhenRowIsInvisibleAfterUpsert() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertThrows(IllegalStateException.class, () -> controller.create(zone(ZONE, "z")));
    }

    @Test
    void updateOverwritesSettingsAndReturnsStoredRow() {
        ZoneConfig body = zone(99L, "renamed"); // 请求体里的 zone_id 不参与定位,以路径为准
        ZoneConfig stored = zone(ZONE, "renamed");
        when(repo.findById(ZONE)).thenReturn(Optional.of(stored));

        ResponseEntity<ZoneConfig> resp = controller.update(ZONE, body);

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody());
        verify(repo).updateSettings(ZONE, body, NOW);
        verify(repo, never()).upsert(any(), any());
    }

    @Test
    void updateDecidesNotFoundByReadBackNotByRowCountAndNeverInserts() {
        // 行数报 1(found rows)也不能当作存在:以同一事务里的回读为准。
        when(repo.updateSettings(eq(ZONE), any(), eq(NOW))).thenReturn(1);
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertEquals(HttpStatus.NOT_FOUND, controller.update(ZONE, zone(ZONE, "z")).getStatusCode());
        verify(repo, never()).upsert(any(), any());
    }

    @Test
    void updateRejectsMissingNameWithoutTouchingDb() {
        assertEquals(HttpStatus.BAD_REQUEST, controller.update(ZONE, zone(ZONE, null)).getStatusCode());
        verifyNoInteractions(repo);
    }

    @Test
    void setMaintenanceKeepsMessageWhenBodyOrMessageAbsent() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.OK,
                controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest(null)).getStatusCode());

        verify(repo, times(2)).updateManualStatus(ZONE, 1, null, NOW);
    }

    @Test
    void setMaintenanceWritesGivenMessage() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        controller.setMaintenance(ZONE, new AdminZoneController.MaintenanceRequest("14:00 恢复"));

        verify(repo).updateManualStatus(ZONE, 1, "14:00 恢复", NOW);
    }

    @Test
    void setOpenClearsMessage() {
        when(repo.findById(ZONE)).thenReturn(Optional.of(zone(ZONE, "z")));

        assertEquals(HttpStatus.OK, controller.setOpen(ZONE).getStatusCode());
        verify(repo).updateManualStatus(ZONE, 0, "", NOW);
    }

    @Test
    void manualStatusActionsReturnNotFoundWhenRowAbsent() {
        when(repo.findById(ZONE)).thenReturn(Optional.empty());

        assertEquals(HttpStatus.NOT_FOUND, controller.setMaintenance(ZONE, null).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.setOpen(ZONE).getStatusCode());
        verify(repo, never()).upsert(any(), any());
    }

    @Test
    void updatePathsDoNotRetryDeadlockClassifiedFailures() {
        // update 路径只有一把锁、拿到后不再等,不可能是死锁牺牲者;万一底层报了,也原样抛出,不做重试。
        when(repo.updateManualStatus(anyLong(), anyInt(), any(), any())).thenThrow(deadlock());

        assertThrows(CannotAcquireLockException.class, () -> controller.setOpen(ZONE));
        verify(repo, times(1)).updateManualStatus(anyLong(), anyInt(), any(), any());
    }

    @Test
    void deleteMapsAffectedRowsToNoContentOrNotFound() {
        when(repo.deleteByZoneId(ZONE)).thenReturn(1, 0);

        assertEquals(HttpStatus.NO_CONTENT, controller.delete(ZONE).getStatusCode());
        assertEquals(HttpStatus.NOT_FOUND, controller.delete(ZONE).getStatusCode());
        verify(repo, times(2)).deleteByZoneId(ZONE);
        verifyNoMoreInteractions(repo);
    }

    private static CannotAcquireLockException deadlock() {
        // Connector/J 对 1213 抛的就是 SQLTransactionRollbackException 的子类。
        return new CannotAcquireLockException("deadlock", new SQLTransactionRollbackException(
                "Deadlock found when trying to get lock; try restarting transaction", "40001", 1213));
    }

    private static ZoneConfig zone(Long zoneId, String name) {
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(zoneId);
        z.setName(name);
        return z;
    }
}
