package com.game.gateway.repository;

import com.game.gateway.entity.ZoneWhitelist;
import org.springframework.data.jpa.repository.Modifying;
import org.springframework.data.jpa.repository.Query;
import org.springframework.data.repository.Repository;
import org.springframework.data.repository.query.Param;
import org.springframework.transaction.annotation.Transactional;

import java.util.List;
import java.util.Optional;

/**
 * zone_whitelist 的读写。
 *
 * <h2>写路径只有两条,凡是会与别人冲突的锁都按「uk_zone_account 项 → 已有主键记录」同向取(2026-09-21 死锁审计 #18)</h2>
 * 这张表的业务键是唯一索引 {@code uk_zone_account(zone_id, account_id)},{@code id} 只是自增代理主键。
 * 写入一律按业务键定位:{@link #upsert} 与 {@link #deleteByZoneIdAndAccountId}。
 * <ul>
 *   <li>DELETE:先在唯一索引上取 X,再回表取已有主键记录的 X。</li>
 *   <li>upsert 走<b>插入</b>分支(键不存在或只剩删除标记项):InnoDB 先插聚簇索引 —— 一条新自增主键记录,只带
 *       隐式锁,并在主键尾部间隙取插入意向 —— 然后才对 uk_zone_account 做重复键检查(对候选项及其后一项取 X
 *       next-key),最后插入 uk 项。</li>
 *   <li>upsert 走<b>重复键</b>分支(键已有活行):先把刚插的新主键记录在语句内回滚,再按 uk 项 X → 已有主键 X
 *       改 note,与 DELETE 同向。</li>
 * </ul>
 * 插入分支里那条新主键记录不参与任何锁序比较:它的 id 刚分配、只有本事务知道,没有写者会按主键或主键范围去锁它
 * (别人只可能经由 uk 项碰到它,那已是 uk 上的排队)。所以两条写路径彼此只会排队、不会成环。
 *
 * <p>刻意继承 {@link Repository} 而不是 {@code JpaRepository}:不再暴露 {@code save()} / {@code delete(entity)} /
 * {@code deleteById()}。{@code save()} 正是 #18 的成因(见 {@link #upsert}),{@code deleteById()} 与 merge 式
 * {@code save()} 则是「先主键、后唯一键」的反向取锁;把它们从接口上拿掉,比在注释里写「不要用」更拦得住回归。
 */
public interface ZoneWhitelistRepository extends Repository<ZoneWhitelist, Long> {

    /**
     * {@link #upsert} 的语句。抽成常量,是为了让真库锁序回归用例执行与生产**同一份**文本,不另抄一份。
     *
     * <h3>为什么用 {@code VALUES(note)} 而不是行别名 {@code AS new … note = new.note}</h3>
     * <ul>
     *   <li>部署口径:k8s 是 {@code mysql:8.0}(浮动 tag,见 deploy/k8s/manifests/infra/mysql.yaml)、compose 是
     *       {@code mysql:latest},数据层迁移目标是 TiDB v8.5(docs/design/global-data-layer-tidb-decision.md、
     *       deploy/docker-compose.tidb.yml)。行别名要求 MySQL 8.0.19+,而 TiDB 的 INSERT 语法里没有行别名子句
     *       (按 TiDB 语法文档,本机未实测);{@code VALUES(col)} 两边都认。</li>
     *   <li>全仓既有 ODKU(go/friend、go/guild、proto2mysql 生成的语句、本模块 seed_stress_3zones.sql)
     *       一律是 {@code VALUES(col)} 或自引用写法,没有一处用行别名,这里保持一致。</li>
     *   <li>代价:MySQL 8.0.20 起 {@code VALUES()} 在 ODKU 里标记弃用,执行会带 1287 告警,但语义不变、仍可用。
     *       哪天 MySQL 真移除它,须全仓一起换写法,不要只改这一处。</li>
     * </ul>
     */
    String UPSERT_SQL = "INSERT INTO zone_whitelist (zone_id, account_id, note) VALUES (:zoneId, :accountId, :note)"
            + " ON DUPLICATE KEY UPDATE note = VALUES(note)";

    /** {@link #deleteByZoneIdAndAccountId} 的语句(JPQL)。Hibernate 翻译成按 uk_zone_account 定位的单条 DELETE。 */
    String DELETE_BY_KEY_JPQL = "DELETE FROM ZoneWhitelist w WHERE w.zoneId = :zoneId AND w.accountId = :accountId";

    List<ZoneWhitelist> findByZoneId(Long zoneId);

    boolean existsByZoneIdAndAccountId(Long zoneId, Long accountId);

    /** 按业务键点查。普通一致性读,不加锁。 */
    Optional<ZoneWhitelist> findByZoneIdAndAccountId(Long zoneId, Long accountId);

    /**
     * 按业务键幂等写入:不存在则插入,已存在则只改 note。返回值是驱动报告的行数,调用方不应依赖它区分插入与更新
     * (Connector/J 默认报 found rows,「插入」与「note 未变的更新」都是 1)。
     *
     * <h3>为什么不用 JPA 的 save()(审计 #18 的环)</h3>
     * 普通 INSERT 在唯一二级索引上做重复键检查时,对「可能重复」的记录 —— **包括已删除标记(delete-marked)
     * 但尚未 purge 的旧记录** —— 以及其后一条记录加 <b>S</b> next-key 锁。时序:
     * <ol>
     *   <li>T1 = remove 已执行 DELETE、未提交:持有旧行主键与 uk 项 (z,a,旧id) 的 X;</li>
     *   <li>T2、T3 = 两个同键 add:各自在 (z,a,旧id) 上请求 S,排在 T1 后面;</li>
     *   <li>T1 提交:S 与 S 相容,T2、T3 <b>同时</b>拿到 S(连同其后记录上的 S next-key);旧记录是已提交的删除,
     *       不算重复,两者都要把新项插进同一个间隙,插入意向锁(X 类)被对方持有的 S 间隙挡住 → 互等 → 1213。</li>
     * </ol>
     * 先到的 add 因故回滚时,排队的两个 add 也会以同样方式成环(MySQL 手册「Locks Set by Different SQL
     * Statements」的 DELETE + 双 INSERT 例)。
     *
     * <h3>为什么 ODKU 只会排队</h3>
     * ODKU 的重复键检查直接取 <b>X</b> next-key,不存在「先拿 S、再升级」这一步。X 与 X 互斥,等待者只能一个一个拿到:
     * 先到者插入(或改 note)并提交,后到者再拿到锁时看见的是活行,走 UPDATE 分支。锁序按分支不同(见类注释):
     * 插入分支是「新主键记录(隐式锁,别人碰不到)→ uk 重复键检查 X → 插入 uk 项」,重复键分支是
     * 「uk 项 X → 已有主键 X」;能与别人冲突的部分都与 {@link #deleteByZoneIdAndAccountId} 同向。
     * 走 UPDATE 分支也会消耗一个自增号,id 出现空洞无害(id 只是代理主键,接口一律按业务键定位)。
     *
     * <h3>为什么不靠 JDBC URL 加 READ COMMITTED</h3>
     * InnoDB 对唯一二级索引的重复键检查在 RC 下仍然加 S next-key(那把锁是唯一约束本身要的),RC 去不掉它,
     * 也就拆不开上面的环;改连接级隔离还会波及本服务所有读写,是全局副作用。环只能靠换语句拆。
     *
     * <h3>剩下去不掉的固有情形</h3>
     * 同键并发插入且先到者<b>回滚</b>,或 purge 恰在排队期间清掉删除标记的 uk 项时,排队者的锁被继承成后继记录上的
     * 间隙锁,InnoDB 仍可能牺牲一个等待者(1213)。这由调用方(AdminWhitelistController#add)做有上限的整事务
     * 重试兜住,收敛论证见 AdminWhitelistController#executeRetryingDeadlockVictim。
     *
     * <p>native 语句绕过持久化上下文,所以先 flush 再 clear,免得同一事务里随后读到缓存里的旧实体。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(value = UPSERT_SQL, nativeQuery = true)
    int upsert(@Param("zoneId") Long zoneId, @Param("accountId") Long accountId, @Param("note") String note);

    /**
     * 按业务键删除,返回删掉的行数(0 = 本来就不在)。
     *
     * <p>单条语句,不再是派生删除的「先 SELECT 取实体、再按主键 DELETE」:取锁顺序因此是「uk 项 → 主键」,与
     * {@link #upsert} 一致。RR 下删一个不存在的键只拿间隙锁;事务里只有这一条语句、随即提交,
     * 不会在持锁的同时再去等别的锁,构不成环。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(DELETE_BY_KEY_JPQL)
    int deleteByZoneIdAndAccountId(@Param("zoneId") Long zoneId, @Param("accountId") Long accountId);
}
