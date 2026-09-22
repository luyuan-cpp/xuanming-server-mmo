package com.game.gateway.controller;

import com.game.gateway.entity.ZoneWhitelist;
import com.game.gateway.repository.ZoneWhitelistRepository;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;
import org.springframework.web.bind.annotation.*;

import java.sql.SQLException;
import java.util.List;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.Supplier;

@RestController
@RequestMapping("/admin/whitelist")
public class AdminWhitelistController {

    private static final Logger log = LoggerFactory.getLogger(AdminWhitelistController.class);

    /** MySQL ER_LOCK_DEADLOCK(SQLSTATE 40001)。TiDB 悲观事务的死锁回的也是这个码。 */
    static final int MYSQL_ER_LOCK_DEADLOCK = 1213;

    /**
     * {@link #executeRetryingDeadlockVictim} 的总尝试次数(含首次)与退避参数,与 go/shared/assetop 的
     * DefaultTxRetryConfig 同一口径:3 次、10ms 起指数退避、封顶 200ms、±20% 抖动。
     */
    static final int DEADLOCK_MAX_ATTEMPTS = 3;
    private static final long DEADLOCK_BASE_BACKOFF_MS = 10;
    private static final long DEADLOCK_MAX_BACKOFF_MS = 200;

    private final ZoneWhitelistRepository whitelistRepo;
    private final TransactionOperations tx;

    public AdminWhitelistController(ZoneWhitelistRepository whitelistRepo, TransactionOperations tx) {
        this.whitelistRepo = whitelistRepo;
        this.tx = tx;
    }

    @GetMapping("/{zoneId}")
    public List<ZoneWhitelist> listByZone(@PathVariable Long zoneId) {
        return whitelistRepo.findByZoneId(zoneId);
    }

    /**
     * 把 (zone_id, account_id) 加进白名单,返回库里的那一行。成功时仍是 200 + ZoneWhitelist JSON,返回形状不变。
     *
     * <p>相对 2026-09-21 之前的 {@code save(entry)},以下变化都是<b>有意</b>的(死锁审计 #18):
     * <ul>
     *   <li><b>幂等</b>:同一 (zone_id, account_id) 重复 add 不再因唯一键冲突回 500,而是回 200 + 已有那一行,
     *       note 以最后一次 add 为准。写入改用 ODKU 才拆得掉 #18 的环(见 {@link ZoneWhitelistRepository#upsert}),
     *       ODKU 天然就是幂等语义。</li>
     *   <li><b>忽略请求体里的 id</b>,只按业务键定位。旧写法带 id 时 save() 走 merge,会按主键把那一行改成另一组
     *       (zone_id, account_id) —— 既是越过业务键改数据,也是「先主键、后唯一键」的反向取锁。</li>
     *   <li>缺 zone_id / account_id 回 400(旧写法是 Hibernate 非空校验失败,冒成 500)。</li>
     * </ul>
     *
     * <p>upsert 与回读在同一个事务里:upsert 持有该行的 X 直到提交,其间并发的 remove 删不掉它,回读必然命中;
     * 回读是普通一致性读、不加锁,整个事务的锁集与单条 upsert 相同。
     *
     * <p>整事务包在 {@link #executeRetryingDeadlockVictim} 里:ODKU 已拆掉 #18 的环,那里只兜 InnoDB 固有的
     * 1213,收敛论证也在那里。
     */
    @PostMapping
    public ResponseEntity<ZoneWhitelist> add(@RequestBody ZoneWhitelist entry) {
        if (entry.getZoneId() == null || entry.getAccountId() == null) {
            return ResponseEntity.badRequest().build();
        }
        long zoneId = entry.getZoneId();
        long accountId = entry.getAccountId();
        String note = entry.getNote();
        ZoneWhitelist stored = executeRetryingDeadlockVictim(tx, "白名单 add", zoneId, () -> {
            whitelistRepo.upsert(zoneId, accountId, note);
            return whitelistRepo.findByZoneIdAndAccountId(zoneId, accountId)
                    .orElseThrow(() -> new IllegalStateException(
                            "zone_whitelist upsert 之后同一事务内回读不到该行,违反事务可见性,zone_id=" + zoneId));
        });
        return ResponseEntity.ok(stored);
    }

    /**
     * 按业务键删除;本来就不在也回 204,与旧行为一致。
     *
     * <p>删除是 {@link ZoneWhitelistRepository#deleteByZoneIdAndAccountId} 的单条语句、自带事务,控制器不再包事务:
     * 包了也只是同一条语句。不重试:它只取一处业务键的锁,顺序与 upsert 相同(uk 项 → 主键),没有需要兜底的固有情形。
     */
    @DeleteMapping("/{zoneId}/{accountId}")
    public ResponseEntity<Void> remove(@PathVariable Long zoneId, @PathVariable Long accountId) {
        whitelistRepo.deleteByZoneIdAndAccountId(zoneId, accountId);
        return ResponseEntity.noContent().build();
    }

    /**
     * 把 {@code unit} 作为一个完整事务执行;只有它被 InnoDB 选为死锁牺牲者(1213)时,才有上限地<b>整事务</b>重跑。
     *
     * <p>包内共享:{@link AdminZoneController#create} 也走这里 —— 同一个 gateway 库、同一类「ODKU 已拆掉 S→X 之后
     * 剩下的固有情形」、同一重试口径,只保留这一份。按职责它应在独立的支撑类里;2026-09-21 这一轮的改动范围限定在
     * 既有文件内,暂放在最先需要它的这里,挪出去只需改两处调用。
     *
     * <h3>只兜 InnoDB 固有情形,不兜语句写法造成的环</h3>
     * 调用方必须已经把能在 SQL 层拆掉的环拆掉(ODKU 取代普通 INSERT,取锁同向)。剩下去不掉的是:同键并发写入且
     * <b>先到者回滚</b>,或 purge 恰在排队期间清掉删除标记记录 —— 排队者挂在那条记录上的锁被继承成后继记录上的
     * 间隙锁,两个排队者的插入意向互相挡住,InnoDB 牺牲其一。
     *
     * <h3>收敛论证</h3>
     * 牺牲者整事务回滚、手里的锁全部释放,幸存者完成写入并提交;牺牲者重试时撞上的是已提交的活行,走 ODKU 的 UPDATE
     * 分支,只会排队。要再次成环,须再叠上一次「同键并发写入 + 先到者回滚 / purge 恰在窗口内」,每次都是独立的小概率
     * 事件,{@value #DEADLOCK_MAX_ATTEMPTS} 次足以收敛;用尽则原样抛出(500),不吞错、不包装。
     *
     * <p>只重试 1213,不重试 1205(锁等待超时):1205 说明有人持锁超过了 innodb_lock_wait_timeout(默认 50s),
     * 立刻重试只会再等一轮,管理接口的调用方早已超时,直接报错更诚实。
     *
     * <p>线程被中断(例如容器关闭、执行器 shutdownNow)时停止重试并保留中断标记。注意客户端断开连接<b>不会</b>
     * 中断 Servlet 请求线程,所以不能指望「请求取消」让重试停下;好在总退避上限只有约 36ms(10ms + 20ms,含 ±20%
     * 抖动),跑完也无妨。
     *
     * @param opName 只进日志的操作名
     * @param zoneId 只进日志的区服号;不要传 account_id 之类的主体标识(AGENTS.md §11.3)
     */
    static <T> T executeRetryingDeadlockVictim(TransactionOperations tx, String opName, long zoneId, Supplier<T> unit) {
        for (int attempt = 1; ; attempt++) {
            try {
                return tx.execute(status -> unit.get());
            } catch (RuntimeException e) {
                if (attempt >= DEADLOCK_MAX_ATTEMPTS || !isDeadlockVictim(e)) {
                    throw e;
                }
                log.warn("{} 被 InnoDB 选为死锁牺牲者(1213),第 {}/{} 次尝试失败,退避后整事务重试 zone_id={}",
                        opName, attempt, DEADLOCK_MAX_ATTEMPTS, zoneId);
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
        long backoffMs = Math.min(DEADLOCK_BASE_BACKOFF_MS << (failedAttempts - 1), DEADLOCK_MAX_BACKOFF_MS);
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
