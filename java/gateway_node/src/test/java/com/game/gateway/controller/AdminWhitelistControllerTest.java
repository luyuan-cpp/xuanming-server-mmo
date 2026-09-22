package com.game.gateway.controller;

import com.game.gateway.entity.ZoneWhitelist;
import com.game.gateway.repository.ZoneWhitelistRepository;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.dao.CannotAcquireLockException;
import org.springframework.dao.DataIntegrityViolationException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;

import java.sql.SQLException;
import java.sql.SQLTransactionRollbackException;
import java.util.Optional;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.Mockito.*;

/**
 * AdminWhitelistController 的契约测试(死锁审计 #18 的控制器侧)。
 *
 * <p>只验控制器自己的承诺:按业务键 upsert、忽略请求体 id、缺键 400、remove 恒 204、只对 1213 做有上限的重试。
 * 「ODKU 在真库上不成环」是锁行为,由 {@code ZoneWhitelistUpsertLockOrderMySqlTest} 在真 MySQL 上验,这里替代不了。
 *
 * <p>事务用 {@link TransactionOperations#withoutTransaction()} 直通:这里不验事务边界,只验重试把整段
 * 「upsert + 回读」当作一个单元重跑。重试路径会真实退避(10ms、20ms 量级),断言不依赖时长。
 */
class AdminWhitelistControllerTest {

    private static final long ZONE = 7L;
    private static final long ACCOUNT = 42L;

    private ZoneWhitelistRepository repo;
    private AdminWhitelistController controller;

    @BeforeEach
    void setUp() {
        repo = mock(ZoneWhitelistRepository.class);
        controller = new AdminWhitelistController(repo, TransactionOperations.withoutTransaction());
    }

    @AfterEach
    void clearInterruptFlag() {
        // 中断用例会留下中断标记;清掉,免得串到同线程上跑的下一个用例。
        Thread.interrupted();
    }

    @Test
    void addUpsertsByNaturalKeyIgnoresBodyIdAndReturnsStoredRow() {
        ZoneWhitelist stored = row(1001L, "qa");
        when(repo.findByZoneIdAndAccountId(ZONE, ACCOUNT)).thenReturn(Optional.of(stored));

        ZoneWhitelist body = request(ZONE, ACCOUNT, "qa");
        body.setId(999L); // 请求体里的 id 必须被忽略

        ResponseEntity<ZoneWhitelist> resp = controller.add(body);

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        assertSame(stored, resp.getBody(), "返回库里那一行,而不是回显请求体");
        verify(repo).upsert(ZONE, ACCOUNT, "qa");
        verify(repo).findByZoneIdAndAccountId(ZONE, ACCOUNT);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void addRejectsMissingNaturalKeyWithoutTouchingDb() {
        assertEquals(HttpStatus.BAD_REQUEST, controller.add(request(null, ACCOUNT, "x")).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST, controller.add(request(ZONE, null, "x")).getStatusCode());
        verifyNoInteractions(repo);
    }

    @Test
    void addRetriesDeadlockVictimAsWholeUnitThenSucceeds() {
        when(repo.upsert(ZONE, ACCOUNT, "n"))
                .thenThrow(deadlock())
                .thenReturn(1);
        when(repo.findByZoneIdAndAccountId(ZONE, ACCOUNT)).thenReturn(Optional.of(row(5L, "n")));

        ResponseEntity<ZoneWhitelist> resp = controller.add(request(ZONE, ACCOUNT, "n"));

        assertEquals(HttpStatus.OK, resp.getStatusCode());
        verify(repo, times(2)).upsert(ZONE, ACCOUNT, "n");
        // 第一次 upsert 就失败了,回读只在成功的那一轮发生一次。
        verify(repo, times(1)).findByZoneIdAndAccountId(ZONE, ACCOUNT);
    }

    @Test
    void addGivesUpAfterBoundedAttemptsAndRethrows() {
        CannotAcquireLockException dl = deadlock();
        when(repo.upsert(anyLong(), anyLong(), any())).thenThrow(dl);

        CannotAcquireLockException thrown =
                assertThrows(CannotAcquireLockException.class, () -> controller.add(request(ZONE, ACCOUNT, "n")));

        assertSame(dl, thrown, "用尽重试后原样抛出,不包装、不吞");
        verify(repo, times(AdminWhitelistController.DEADLOCK_MAX_ATTEMPTS)).upsert(ZONE, ACCOUNT, "n");
        verify(repo, never()).findByZoneIdAndAccountId(anyLong(), anyLong());
    }

    @Test
    void addDoesNotRetryLockWaitTimeout() {
        // 1205 同样被 Spring 归成 CannotAcquireLockException;按错误码区分,只重试 1213。
        when(repo.upsert(anyLong(), anyLong(), any())).thenThrow(new CannotAcquireLockException(
                "lock wait timeout", new SQLException("Lock wait timeout exceeded", "HY000", 1205)));

        assertThrows(CannotAcquireLockException.class, () -> controller.add(request(ZONE, ACCOUNT, "n")));
        verify(repo, times(1)).upsert(ZONE, ACCOUNT, "n");
    }

    @Test
    void addDoesNotRetryNonLockFailures() {
        when(repo.upsert(anyLong(), anyLong(), any())).thenThrow(new DataIntegrityViolationException("note too long"));

        assertThrows(DataIntegrityViolationException.class, () -> controller.add(request(ZONE, ACCOUNT, "n")));
        verify(repo, times(1)).upsert(ZONE, ACCOUNT, "n");
    }

    @Test
    void addStopsRetryingWhenRequestThreadIsInterrupted() {
        CannotAcquireLockException dl = deadlock();
        when(repo.upsert(anyLong(), anyLong(), any())).thenThrow(dl);
        Thread.currentThread().interrupt(); // 模拟线程被中断(例如容器关闭):退避时立即醒来

        CannotAcquireLockException thrown =
                assertThrows(CannotAcquireLockException.class, () -> controller.add(request(ZONE, ACCOUNT, "n")));

        assertSame(dl, thrown);
        verify(repo, times(1)).upsert(ZONE, ACCOUNT, "n");
        assertTrue(Thread.currentThread().isInterrupted(), "中断标记必须保留给上层");
    }

    @Test
    void addFailsLoudlyWhenRowIsInvisibleAfterUpsert() {
        when(repo.findByZoneIdAndAccountId(ZONE, ACCOUNT)).thenReturn(Optional.empty());

        assertThrows(IllegalStateException.class, () -> controller.add(request(ZONE, ACCOUNT, "n")));
        verify(repo, times(1)).upsert(ZONE, ACCOUNT, "n");
    }

    @Test
    void removeDeletesByNaturalKeyAndAlwaysReturnsNoContent() {
        when(repo.deleteByZoneIdAndAccountId(ZONE, ACCOUNT)).thenReturn(0);

        assertEquals(HttpStatus.NO_CONTENT, controller.remove(ZONE, ACCOUNT).getStatusCode());
        verify(repo).deleteByZoneIdAndAccountId(ZONE, ACCOUNT);
        verifyNoMoreInteractions(repo);
    }

    @Test
    void deadlockClassifierLooksThroughWrappersAndIgnoresOtherCodes() {
        assertTrue(AdminWhitelistController.isDeadlockVictim(deadlock()));
        assertTrue(AdminWhitelistController.isDeadlockVictim(
                new RuntimeException("outer", new RuntimeException("mid", mysqlDeadlockSqlException()))));
        assertFalse(AdminWhitelistController.isDeadlockVictim(
                new RuntimeException(new SQLException("dup", "23000", 1062))));
        assertFalse(AdminWhitelistController.isDeadlockVictim(new RuntimeException("no sql cause")));
    }

    private static CannotAcquireLockException deadlock() {
        return new CannotAcquireLockException("deadlock", mysqlDeadlockSqlException());
    }

    private static SQLException mysqlDeadlockSqlException() {
        // Connector/J 对 1213 抛的就是 SQLTransactionRollbackException 的子类。
        return new SQLTransactionRollbackException(
                "Deadlock found when trying to get lock; try restarting transaction", "40001", 1213);
    }

    private static ZoneWhitelist request(Long zoneId, Long accountId, String note) {
        ZoneWhitelist w = new ZoneWhitelist();
        w.setZoneId(zoneId);
        w.setAccountId(accountId);
        w.setNote(note);
        return w;
    }

    private static ZoneWhitelist row(long id, String note) {
        ZoneWhitelist w = request(ZONE, ACCOUNT, note);
        w.setId(id);
        return w;
    }
}
