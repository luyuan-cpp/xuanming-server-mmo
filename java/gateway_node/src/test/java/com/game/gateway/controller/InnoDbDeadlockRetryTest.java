package com.game.gateway.controller;

import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;
import org.springframework.dao.CannotAcquireLockException;
import org.springframework.dao.DataIntegrityViolationException;
import org.springframework.transaction.support.TransactionOperations;

import java.sql.SQLException;
import java.sql.SQLTransactionRollbackException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * {@link InnoDbDeadlockRetry} 的契约测试:死锁牺牲者的识别,以及「整单元重跑、次数有上限、放弃时原样抛出」。
 *
 * <p>这些断言原先散在 {@code AdminWhitelistControllerTest} 里(重试逻辑当时是白名单控制器的包内静态方法);
 * 支撑类独立出来之后,契约跟着搬到这里,不再借某一个控制器去验。两个控制器各自的端到端重试行为仍由
 * {@code AdminWhitelistControllerTest} / {@code AdminZoneControllerTest} 钉住。
 *
 * <p>事务用 {@link TransactionOperations#withoutTransaction()} 直通:这里不验事务边界,只验重试次数与异常传递。
 * 重试路径会真实退避(10ms、20ms 量级),断言不依赖时长。
 */
class InnoDbDeadlockRetryTest {

    private final TransactionOperations tx = TransactionOperations.withoutTransaction();

    @AfterEach
    void clearInterruptFlag() {
        // 中断用例会留下中断标记;清掉,免得串到同线程上跑的下一个用例。
        Thread.interrupted();
    }

    @Test
    void classifierLooksThroughWrappersAndIgnoresOtherCodes() {
        assertTrue(InnoDbDeadlockRetry.isDeadlockVictim(deadlock()));
        assertTrue(InnoDbDeadlockRetry.isDeadlockVictim(
                new RuntimeException("outer", new RuntimeException("mid", mysqlDeadlockSqlException()))));
        assertFalse(InnoDbDeadlockRetry.isDeadlockVictim(
                new RuntimeException(new SQLException("dup", "23000", 1062))));
        assertFalse(InnoDbDeadlockRetry.isDeadlockVictim(new RuntimeException("no sql cause")));
        assertFalse(InnoDbDeadlockRetry.isDeadlockVictim(null));
    }

    /**
     * 环形 cause 链不许把分类器转成死循环 —— 自环({@code getCause() == this})与 A→B→A 两节点环都要钉住。
     *
     * <p>两节点环是这条用例的重点:{@code Throwable.initCause} 只拒绝 {@code cause == this},A→B→A 构造得出来,
     * 而「只判自环」的旧写法(一个三目)挡不住它,{@link InnoDbDeadlockRetry#isDeadlockVictim} 会永不返回。分类器
     * 跑在 Servlet 请求线程上(客户端断开不会中断它),死循环就是一个永不返回的 /admin 请求,所以这里要的是
     * 「返回 false」而不只是「不崩」。现在的实现改成了沿 cause 链的深度上限,自环与任意长度的环一并覆盖。
     *
     * <p>{@code threadMode = SEPARATE_THREAD} 不是装饰:回归(去掉深度上限)时本用例的表现是<b>挂死</b>而不是
     * 失败,而 {@code @Timeout} 默认的 SAME_THREAD 只在用例<b>跑完之后</b>比时长 —— 永不返回就永远比不上,CI 会
     * 卡住而不是变红。SEPARATE_THREAD 到点直接判红(JUnit 5.9+)。
     */
    @Test
    @Timeout(value = 5, unit = TimeUnit.SECONDS, threadMode = Timeout.ThreadMode.SEPARATE_THREAD)
    void classifierSurvivesCyclicCauseChain() {
        SQLException selfCaused = new SQLException("self", "HY000", 1062) {
            @Override
            public synchronized Throwable getCause() {
                return this;
            }
        };
        assertFalse(InnoDbDeadlockRetry.isDeadlockVictim(selfCaused));

        SQLException a = new SQLException("a", "HY000", 1062);
        SQLException b = new SQLException("b", "HY000", 1062, a);
        a.initCause(b); // 构造 A→B→A:a 是用三参构造器建的,cause 尚未初始化,initCause 只拒绝 cause == this
        assertFalse(InnoDbDeadlockRetry.isDeadlockVictim(a), "两节点环必须走完深度上限后判为非死锁,不得死循环");
    }

    @Test
    void returnsUnitResultWithoutRetryingOnSuccess() {
        AtomicInteger calls = new AtomicInteger();
        String got = InnoDbDeadlockRetry.execute(tx, "成功路径", 7L, () -> {
            calls.incrementAndGet();
            return "ok";
        });

        assertEquals("ok", got);
        assertEquals(1, calls.get(), "没失败就不该重跑");
    }

    @Test
    void retriesWholeUnitOnDeadlockThenSucceeds() {
        AtomicInteger calls = new AtomicInteger();
        String got = InnoDbDeadlockRetry.execute(tx, "第一次是牺牲者", 7L, () -> {
            if (calls.incrementAndGet() == 1) {
                throw deadlock();
            }
            return "ok";
        });

        assertEquals("ok", got);
        assertEquals(2, calls.get());
    }

    @Test
    void givesUpAfterBoundedAttemptsAndRethrowsSameException() {
        CannotAcquireLockException dl = deadlock();
        AtomicInteger calls = new AtomicInteger();

        CannotAcquireLockException thrown = assertThrows(CannotAcquireLockException.class,
                () -> InnoDbDeadlockRetry.execute(tx, "一直是牺牲者", 7L, () -> {
                    calls.incrementAndGet();
                    throw dl;
                }));

        assertSame(dl, thrown, "用尽重试后原样抛出,不包装、不吞");
        assertEquals(InnoDbDeadlockRetry.MAX_ATTEMPTS, calls.get(), "总尝试次数含首次,不得超过上限");
    }

    @Test
    void doesNotRetryLockWaitTimeout() {
        // 1205 同样被 Spring 归成 CannotAcquireLockException;按错误码区分,只重试 1213。
        AtomicInteger calls = new AtomicInteger();
        CannotAcquireLockException timeout = new CannotAcquireLockException(
                "lock wait timeout", new SQLException("Lock wait timeout exceeded", "HY000", 1205));

        assertThrows(CannotAcquireLockException.class,
                () -> InnoDbDeadlockRetry.execute(tx, "锁等待超时", 7L, () -> {
                    calls.incrementAndGet();
                    throw timeout;
                }));

        assertEquals(1, calls.get());
    }

    @Test
    void doesNotRetryNonLockFailures() {
        AtomicInteger calls = new AtomicInteger();

        assertThrows(DataIntegrityViolationException.class,
                () -> InnoDbDeadlockRetry.execute(tx, "约束失败", 7L, () -> {
                    calls.incrementAndGet();
                    throw new DataIntegrityViolationException("note too long");
                }));

        assertEquals(1, calls.get());
    }

    @Test
    void stopsRetryingWhenThreadIsInterruptedAndKeepsTheFlag() {
        CannotAcquireLockException dl = deadlock();
        AtomicInteger calls = new AtomicInteger();
        Thread.currentThread().interrupt(); // 模拟线程被中断(例如容器关闭):退避时立即醒来

        CannotAcquireLockException thrown = assertThrows(CannotAcquireLockException.class,
                () -> InnoDbDeadlockRetry.execute(tx, "中断", 7L, () -> {
                    calls.incrementAndGet();
                    throw dl;
                }));

        assertSame(dl, thrown);
        assertEquals(1, calls.get(), "中断后不再重跑");
        assertTrue(Thread.currentThread().isInterrupted(), "中断标记必须保留给上层");
    }

    private static CannotAcquireLockException deadlock() {
        return new CannotAcquireLockException("deadlock", mysqlDeadlockSqlException());
    }

    private static SQLException mysqlDeadlockSqlException() {
        // Connector/J 对 1213 抛的就是 SQLTransactionRollbackException 的子类。
        return new SQLTransactionRollbackException(
                "Deadlock found when trying to get lock; try restarting transaction", "40001", 1213);
    }
}
