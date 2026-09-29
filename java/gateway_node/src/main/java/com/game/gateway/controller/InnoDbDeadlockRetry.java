package com.game.gateway.controller;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.transaction.support.TransactionOperations;

import java.sql.SQLException;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.Supplier;

/**
 * 「整事务重跑 InnoDB 死锁牺牲者」的有界重试(2026-09-21 死锁审计 #18 及同类项)。
 *
 * <p>管理接口的写路径共用这一份:{@link AdminWhitelistController#add} 与 {@link AdminZoneController#create}
 * 面对的是同一个 gateway 库、同一类「ODKU 已拆掉 S→X 之后剩下的 InnoDB 固有情形」、同一套重试口径。
 * 之前它是 {@code AdminWhitelistController} 的包内静态方法,区服控制器跨控制器调用它、告警日志还打在白名单
 * 控制器的 logger 上;现在独立成支撑类,两个控制器都依赖它,谁也不依赖谁。
 *
 * <p>本类只做「重试」这一件事:它不知道被包的单元在写什么表,也不碰返回码与异常类型 —— 成功原样返回,
 * 放弃时原样抛出。
 *
 * <h2>只兜 InnoDB 固有情形,不兜语句写法造成的环</h2>
 * 调用方必须已经把能在 SQL 层拆掉的环拆掉(ODKU 取代普通 INSERT,取锁同向)。剩下去不掉的是:同键并发写入且
 * <b>先到者回滚</b>,或 purge 恰在排队期间清掉删除标记记录 —— 排队者挂在那条记录上的锁被继承成后继记录上的
 * 间隙锁,两个排队者的插入意向互相挡住,InnoDB 牺牲其一。
 *
 * <h2>收敛论证</h2>
 * 牺牲者整事务回滚、手里的锁全部释放,幸存者完成写入并提交;牺牲者重试时撞上的是已提交的活行,走 ODKU 的 UPDATE
 * 分支,只会排队。要再次成环,须再叠上一次「同键并发写入 + 先到者回滚 / purge 恰在窗口内」,每次都是独立的小概率
 * 事件,{@value #MAX_ATTEMPTS} 次足以收敛;用尽则原样抛出(500),不吞错、不包装。
 *
 * <p>只重试 1213,不重试 1205(锁等待超时):1205 说明有人持锁超过了 innodb_lock_wait_timeout(默认 50s),
 * 立刻重试只会再等一轮,管理接口的调用方早已超时,直接报错更诚实。
 *
 * <p>线程被中断(例如容器关闭、执行器 shutdownNow)时停止重试并保留中断标记。注意客户端断开连接<b>不会</b>
 * 中断 Servlet 请求线程,所以不能指望「请求取消」让重试停下;好在总退避上限只有约 36ms(10ms + 20ms,含 ±20%
 * 抖动),跑完也无妨。
 */
final class InnoDbDeadlockRetry {

    private static final Logger log = LoggerFactory.getLogger(InnoDbDeadlockRetry.class);

    /** MySQL ER_LOCK_DEADLOCK(SQLSTATE 40001)。TiDB 悲观事务的死锁回的也是这个码。 */
    private static final int MYSQL_ER_LOCK_DEADLOCK = 1213;

    /**
     * {@link #execute} 的总尝试次数(含首次)与退避参数,与 go/shared/assetop 的 DefaultTxRetryConfig
     * 同一口径:3 次、10ms 起指数退避、封顶 200ms、±20% 抖动。
     */
    static final int MAX_ATTEMPTS = 3;
    private static final long BASE_BACKOFF_MS = 10;
    private static final long MAX_BACKOFF_MS = 200;

    private InnoDbDeadlockRetry() {
    }

    /**
     * 把 {@code unit} 作为一个完整事务执行;只有它被 InnoDB 选为死锁牺牲者(1213)时,才有上限地<b>整事务</b>重跑。
     *
     * @param tx     事务边界;{@code unit} 的每一次尝试都跑在自己的事务里
     * @param opName 只进日志的操作名
     * @param zoneId 只进日志的区服号;不要传 account_id 之类的主体标识(AGENTS.md §11.3)
     * @param unit   要整体重跑的单元:必须幂等(ODKU + 同事务回读天然满足)
     * @return {@code unit} 的返回值
     */
    static <T> T execute(TransactionOperations tx, String opName, long zoneId, Supplier<T> unit) {
        for (int attempt = 1; ; attempt++) {
            try {
                return tx.execute(status -> unit.get());
            } catch (RuntimeException e) {
                if (attempt >= MAX_ATTEMPTS || !isDeadlockVictim(e)) {
                    throw e;
                }
                log.warn("{} 被 InnoDB 选为死锁牺牲者(1213),第 {}/{} 次尝试失败,退避后整事务重试 zone_id={}",
                        opName, attempt, MAX_ATTEMPTS, zoneId);
                if (!sleepBeforeRetry(attempt)) {
                    throw e;
                }
            }
        }
    }

    /** 异常链上任何一层是 MySQL 1213 即算死锁牺牲者;不依赖 Spring / Hibernate 把它翻译成哪个异常类。 */
    static boolean isDeadlockVictim(Throwable e) {
        for (Throwable t = e; t != null; t = t.getCause() == t ? null : t.getCause()) {
            if (t instanceof SQLException sql && sql.getErrorCode() == MYSQL_ER_LOCK_DEADLOCK) {
                return true;
            }
        }
        return false;
    }

    /**
     * 第 failedAttempts 次失败后的退避。返回 false 表示线程被中断(例如容器关闭):恢复中断标记,调用方停止重试。
     */
    private static boolean sleepBeforeRetry(int failedAttempts) {
        long backoffMs = Math.min(BASE_BACKOFF_MS << (failedAttempts - 1), MAX_BACKOFF_MS);
        long jitteredMs = Math.round(backoffMs * ThreadLocalRandom.current().nextDouble(0.8, 1.2));
        try {
            Thread.sleep(jitteredMs);
            return true;
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
            return false;
        }
    }
}
