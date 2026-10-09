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
 * 调用方必须已经把能在 SQL 层拆掉的环拆掉,并且<b>对自己那张表</b>拿得出证据。剩下去不掉的是:同键并发写入且
 * <b>先到者回滚</b>,或 purge 恰在排队期间清掉删除标记记录 —— 排队者挂在那条记录上的锁被继承成后继记录上的
 * 间隙锁,两个排队者的插入意向互相挡住,InnoDB 牺牲其一。
 *
 * <p><b>「ODKU 取代普通 INSERT」不是一条通用的消环结论</b>,而且<b>本类两个调用方能只兜「固有情形」的理由并不
 * 相同</b> —— 别把其中一个的理由安给另一个。下面按 2026-09-29 的真库探针(MySQL 26.7.0,全局 RR,
 * {@code innodb_deadlock_detect=ON},编排与本包的 {@code MySqlLockOrderFixture} 同形,每格重复 5 次)逐条写明:
 * <ul>
 *   <li>{@link AdminWhitelistController#add} 写的 {@code zone_whitelist}:自增代理主键 + 业务唯一索引
 *       {@code uk_zone_account},争抢的键是<b>二级唯一索引</b>,新行的唯一键项必然排在删除标记项<b>之后</b>
 *       —— 这一路 ODKU <b>0/5</b> 成环;</li>
 *   <li>{@link AdminZoneController#create} 写的 {@code zone_config}:它走的是<b>另一条</b>安全路径,与「新项排在
 *       后面」毫无关系。这张表只有 {@code zone_id INT UNSIGNED PRIMARY KEY}(见 schema.sql),主键由请求体传入、
 *       <b>不是</b>自增,也没有任何二级唯一索引;争抢的键就是<b>聚簇主键</b>,而 {@link
 *       com.game.gateway.repository.ZoneConfigRepository#UPSERT_SQL} 是 ODKU —— 两个条件凑齐,重复键才走原地改写。
 *       探针 {@code probe-results.md} 第 54 行正是把它与 {@code trade_favorite}、friend 各表归在这一类;</li>
 *   <li>反例 ——「新行主键<b>小于</b>删除标记那条记录的主键」的形状(go/login 的 {@code player_name}:主键 player_id +
 *       唯一索引 name_norm,号段发的新 id 小于存量 snowflake id):同一条 ODKU <b>5/5 成环</b>。</li>
 * </ul>
 * 新的调用方接进来之前,必须先按自己那张表的主键生成方式重新判一次,不能因为这里写着「有重试兜底」就把它当成
 * 安全网 —— 能消的环必须在 SQL 层消掉(AGENTS.md §11.3)。判据有两条,<b>满足其一</b>即可:
 * <ol>
 *   <li>争抢的键<b>就是聚簇主键</b>,<b>且</b>写法是 ODKU(直接取 X、不走 S→X)—— {@code zone_config} 这一路,
 *       <b>两个条件缺一不可</b>。「重复键撞上原地改写、不经过二级唯一索引的重复键检查」是这两个条件<b>凑齐之后
 *       的结果</b>,不是条件本身:同一张聚簇主键的表配<b>普通 INSERT</b>,探针三种主键顺序全是 <b>5/5 成环</b>
 *       (聚簇记录上的重复检查取 S,两个 S 同时被授予之后各自要升级成 X,互相挡住 —— 手册那个三会话例就建在
 *       聚簇主键上);换成 ODKU 才是 <b>0/5</b>。见 {@code probe-results.md} 第 36-38 行;</li>
 *   <li>争抢的键是二级唯一索引,<b>新行的唯一键项必然排在删除标记项之后</b>,<b>且</b>写法是 ODKU(直接取 X、
 *       不走 S→X)—— {@code zone_whitelist} 这一路,同样<b>两个条件缺一不可</b>。</li>
 * </ol>
 * 两条都不满足的表落在上面的反例那一路,环消不掉,重试兜不住。
 *
 * <h2>收敛论证</h2>
 * 牺牲者整事务回滚、手里的锁全部释放,幸存者完成写入并提交;牺牲者重试时撞上的是已提交的活行,走 ODKU 的 UPDATE
 * 分支,只会排队。要再次成环,须再叠上一次「同键并发写入 + 先到者回滚 / purge 恰在窗口内」,每次都是独立的小概率
 * 事件,{@value #MAX_ATTEMPTS} 次足以收敛;用尽则原样抛出(500),不吞错、不包装。
 *
 * <p>这段论证<b>只对满足上面判据 1 或 2 的表成立</b>(即当前两个调用方各自那一路),对反例那一路不成立;而且它
 * <b>至今没有真库证据</b>:两个调用方的表上都还没有观察到过一次 1213 被重试吸收。别把它当成已验证的结论。
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

    /** {@link #isDeadlockVictim} 沿 cause 链最多走多少层;防的是环,不是深链(理由见该方法)。 */
    private static final int MAX_CAUSE_DEPTH = 32;

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

    /**
     * 异常链上任何一层是 MySQL 1213 即算死锁牺牲者;不依赖 Spring / Hibernate 把它翻译成哪个异常类。
     *
     * <p>遍历有<b>深度上限</b>({@value #MAX_CAUSE_DEPTH}):{@code Throwable.initCause} 只拒绝 {@code cause == this},
     * A→B→A 这种两节点环是能构造出来的,而本方法跑在 Servlet 请求线程上(客户端断开不会中断它),一旦转成死循环
     * 就是一个永不返回的 /admin 请求。深度上限同时覆盖自环与任意长度的环,比「只判自环」的写法更简单也更完整
     * (JDK 自己的 {@code printStackTrace} 用 seen-set,同一意图)。观察到的 Connector/J + Spring 异常链只有几层,
     * {@value #MAX_CAUSE_DEPTH} 远高于此,误伤在实践中没见过(<b>未实测</b>,没有量过真实链长的分布);真被截断时
     * 按「不是死锁」处理 —— 不重试,错误原样抛出,方向偏保守(代价是第 {@value #MAX_CAUSE_DEPTH} 层以外的 1213
     * 会被判成非死锁 → 不重试 → 500)。
     */
    static boolean isDeadlockVictim(Throwable e) {
        Throwable t = e;
        for (int depth = 0; t != null && depth < MAX_CAUSE_DEPTH; t = t.getCause(), depth++) {
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
