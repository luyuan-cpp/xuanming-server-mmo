package com.game.gateway.repository;

import com.game.gateway.entity.ZoneConfig;
import org.springframework.data.jpa.repository.Modifying;
import org.springframework.data.jpa.repository.Query;
import org.springframework.data.repository.Repository;
import org.springframework.data.repository.query.Param;
import org.springframework.transaction.annotation.Transactional;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Optional;

/**
 * zone_config(选服目录)的读写。
 *
 * <h2>写路径:每条都是「按主键的单条语句」,只锁一条主键记录,且没有 S→X 升级(2026-09-21 死锁审计 #18 同类项)</h2>
 * zone_id 是业务赋值的主键(实体上没有生成策略),表上没有二级索引。写入只有四条:{@link #upsert}(ODKU)、
 * {@link #updateSettings}、{@link #updateManualStatus}、{@link #deleteByZoneId}。每条语句在 InnoDB 里最多取
 * <b>一把</b>行锁 —— 主键记录上的 X(记录不在时是一把间隙锁,间隙锁从不等待)—— 而且是它所在事务的第一把锁;
 * 调用方事务里其后只有非锁定的一致性读回读。一个事务「等锁时手里什么都没有、拿到锁之后再不等」,就不可能出现在
 * 任何等待环上,所以这四条之间、以及与任何别的写者之间都只会排队。
 *
 * <p>刻意继承 {@link Repository} 而不是 {@code JpaRepository}:不再暴露 {@code save()} / {@code deleteById()}。
 * 对业务赋值主键、又没有 {@code @Version} 的实体,{@code save()} 永远走 merge:先非锁定 SELECT,查不到就发普通
 * INSERT。普通 INSERT 在主键重复检查时对已存在或删除标记(delete-marked、尚未 purge)的记录先拿 <b>S</b>,
 * 要「插入」到删除标记记录上时再升级成 X —— 两个这样的 INSERT 同时拿到 S(典型时序:先到的 create 回滚,
 * 排在它后面的两个同键 INSERT 一起被授予 S;或 MySQL 手册「Locks Set by Different SQL Statements」里
 * DELETE + 双 INSERT 的例子)就会互等 1213。merge 还有两个与锁无关的缺陷,一并去掉:
 * <ul>
 *   <li>update / setMaintenance / setOpen 旧写法是 findById + save:两步之间行被删掉时,merge 的 SELECT 查不到,
 *       Hibernate(6.6 起对「无生成主键、无 @Version」的实体仍无法区分新对象与已删对象)会把它<b>重新 INSERT</b> ——
 *       既复活了刚删掉的区服,也让这几条路径成了上面那种普通 INSERT。</li>
 *   <li>findById + save 是整行覆盖:setMaintenance 与并发的 update 交错时,后提交的一方会把对方改过的列用自己
 *       读到的旧快照写回去(丢失更新)。</li>
 * </ul>
 * 读方法按 Spring Data「选择性暴露 CRUD 方法」的约定声明,由 SimpleJpaRepository 实现,行为与原 JpaRepository 相同。
 *
 * <h2>时间戳</h2>
 * native / JPQL 批量语句绕过 {@link ZoneConfig} 的 {@code @PrePersist} / {@code @PreUpdate},所以
 * created_at / updated_at 由调用方显式传入({@code now}),与实体回调同一时钟源(JVM 本地时间,
 * ServerListService 拿 created_at 与 {@code LocalDateTime.now()} 比较「新服」)。不用 MySQL 的 NOW() /
 * ON UPDATE CURRENT_TIMESTAMP:库的会话时区(容器里通常是 UTC)与 JVM 时区可能不同,混用会差出整数个小时。
 */
public interface ZoneConfigRepository extends Repository<ZoneConfig, Long> {

    /**
     * {@link #upsertColumns} 的语句。抽成常量,是为了让真库锁序回归用例执行与生产<b>同一份</b>文本。
     *
     * <p>语义与旧 create 的 {@code save()}(merge)一致:不存在则插入;已存在则<b>覆盖</b>全部业务列
     * (name / manual_status / capacity / maintenance_msg / open_time / recommended / sort_order)并刷新 updated_at,
     * <b>保留 created_at</b>(对应实体上的 {@code updatable = false})。
     *
     * <p>写法 {@code VALUES(col)} 与 {@link ZoneWhitelistRepository#UPSERT_SQL} 同一口径(部署 mysql:8.0 / latest
     * 与迁移目标 TiDB v8.5 都认;行别名写法 TiDB 不认;MySQL 8.0.20 起带 1287 弃用告警但语义不变),理由见那里,
     * 不在这里另选一套。
     */
    String UPSERT_SQL = "INSERT INTO zone_config (zone_id, name, manual_status, capacity, maintenance_msg, open_time,"
            + " recommended, sort_order, created_at, updated_at)"
            + " VALUES (:zoneId, :name, :manualStatus, :capacity, :maintenanceMsg, :openTime,"
            + " :recommended, :sortOrder, :now, :now)"
            + " ON DUPLICATE KEY UPDATE name = VALUES(name), manual_status = VALUES(manual_status),"
            + " capacity = VALUES(capacity), maintenance_msg = VALUES(maintenance_msg), open_time = VALUES(open_time),"
            + " recommended = VALUES(recommended), sort_order = VALUES(sort_order), updated_at = VALUES(updated_at)";

    /** {@link #updateSettingColumns} 的语句(JPQL)。Hibernate 翻译成按主键定位的单条 UPDATE。 */
    String UPDATE_SETTINGS_JPQL = "UPDATE ZoneConfig z SET z.name = :name, z.manualStatus = :manualStatus,"
            + " z.capacity = :capacity, z.maintenanceMsg = :maintenanceMsg, z.openTime = :openTime,"
            + " z.recommended = :recommended, z.sortOrder = :sortOrder, z.updatedAt = :now WHERE z.zoneId = :zoneId";

    /** {@link #updateManualStatus} 的语句(JPQL)。文案参数为 null 时保留原文案。 */
    String UPDATE_MANUAL_STATUS_JPQL = "UPDATE ZoneConfig z SET z.manualStatus = :manualStatus,"
            + " z.maintenanceMsg = COALESCE(:maintenanceMsg, z.maintenanceMsg), z.updatedAt = :now"
            + " WHERE z.zoneId = :zoneId";

    /** {@link #deleteByZoneId} 的语句(JPQL)。Hibernate 翻译成按主键定位的单条 DELETE。 */
    String DELETE_BY_ID_JPQL = "DELETE FROM ZoneConfig z WHERE z.zoneId = :zoneId";

    /** 全部区服。普通一致性读,不加锁。 */
    List<ZoneConfig> findAll();

    /** 按主键点查。普通一致性读,不加锁。 */
    Optional<ZoneConfig> findById(Long zoneId);

    /**
     * 按主键幂等写入整行:见 {@link #UPSERT_SQL}。{@code zone.getZoneId()} 与 {@code zone.getName()} 必须非空
     * (前者是主键,后者列上 NOT NULL;库的 sql_mode 若不含 STRICT,NULL 会被静默改成 '',所以由调用方在接缝处拦)。
     *
     * <h3>为什么 ODKU 只会排队</h3>
     * ODKU 在主键重复检查时对已存在 / 删除标记的记录直接取 <b>X</b>(记录锁,不带间隙),其后「插入到删除标记记录上」
     * 或走 UPDATE 分支要的也是同一把 X —— 没有「先拿 S、再升级」这一步。X 与 X 互斥,同键的并发者只能一个一个拿到:
     * 先到者插入(或覆盖)并提交,后到者拿到锁时看见的是活行,走 UPDATE 分支。键完全不存在时直接插入新记录(隐式锁),
     * 只在插入意向上等别人的间隙锁;而本仓库对这张表唯一会留下间隙锁的写者是「更新 / 删除一个不存在的 zone_id」,
     * 它拿到那把间隙锁之后不再等任何锁,构不成环。
     *
     * <h3>剩下去不掉的固有情形</h3>
     * 同键并发写入且<b>先到者回滚</b>,或 purge 恰在排队期间清掉删除标记记录:排队者挂在那条记录上的锁被 InnoDB
     * 继承成后继记录上的间隙锁,两个排队者的插入意向互相挡住 → 1213。这与语句写法无关,由调用方
     * (AdminZoneController#create)做有上限的整事务重试兜住,收敛论证见那里。
     *
     * <p>返回值是驱动报告的行数,调用方不要拿它区分插入 / 覆盖 / 未变(Connector/J 默认报 found rows),
     * 需要结果时在同一事务里回读。
     */
    default int upsert(ZoneConfig zone, LocalDateTime now) {
        return upsertColumns(zone.getZoneId(), zone.getName(), zone.getManualStatus(), zone.getCapacity(),
                zone.getMaintenanceMsg(), zone.getOpenTime(), zone.isRecommended(), zone.getSortOrder(), now);
    }

    /**
     * {@link #upsert} 的列级形式。调用方用 {@link #upsert},这里只负责绑定。
     *
     * <p>native 语句绕过持久化上下文,所以先 flush 再 clear,免得同一事务里随后读到缓存里的旧实体。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(value = UPSERT_SQL, nativeQuery = true)
    int upsertColumns(@Param("zoneId") Long zoneId, @Param("name") String name,
                      @Param("manualStatus") int manualStatus, @Param("capacity") int capacity,
                      @Param("maintenanceMsg") String maintenanceMsg, @Param("openTime") LocalDateTime openTime,
                      @Param("recommended") boolean recommended, @Param("sortOrder") int sortOrder,
                      @Param("now") LocalDateTime now);

    /**
     * 覆盖已存在区服的全部业务列(不含 created_at),刷新 updated_at;区服不存在时什么也不写,<b>不会</b>插入。
     * 列集合与旧 update 接口逐列一致。{@code values.getName()} 必须非空(理由同 {@link #upsert})。
     *
     * <p>单条按主键的 UPDATE:记录在则取它的 X,不在则只取一把间隙锁;之后调用方只做非锁定回读。
     * 返回值同 {@link #upsert},不要拿它判断存在性(found rows / affected rows 取决于连接参数),在同一事务里回读。
     */
    default int updateSettings(Long zoneId, ZoneConfig values, LocalDateTime now) {
        return updateSettingColumns(zoneId, values.getName(), values.getManualStatus(), values.getCapacity(),
                values.getMaintenanceMsg(), values.getOpenTime(), values.isRecommended(), values.getSortOrder(), now);
    }

    /** {@link #updateSettings} 的列级形式。调用方用 {@link #updateSettings},这里只负责绑定。 */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(UPDATE_SETTINGS_JPQL)
    int updateSettingColumns(@Param("zoneId") Long zoneId, @Param("name") String name,
                             @Param("manualStatus") int manualStatus, @Param("capacity") int capacity,
                             @Param("maintenanceMsg") String maintenanceMsg, @Param("openTime") LocalDateTime openTime,
                             @Param("recommended") boolean recommended, @Param("sortOrder") int sortOrder,
                             @Param("now") LocalDateTime now);

    /**
     * 只改运维状态(与可选的维护文案),刷新 updated_at;其余列不动。{@code maintenanceMsg} 为 null 表示保留原文案。
     * 区服不存在时什么也不写。取锁与返回值口径同 {@link #updateSettings}。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(UPDATE_MANUAL_STATUS_JPQL)
    int updateManualStatus(@Param("zoneId") Long zoneId, @Param("manualStatus") int manualStatus,
                           @Param("maintenanceMsg") String maintenanceMsg, @Param("now") LocalDateTime now);

    /**
     * 按主键删除,返回删掉的行数(0 = 本来就不在)。DELETE 的行数就是真正删掉的行数,不受 found rows 参数影响,
     * 可以直接用来区分 204 / 404。
     *
     * <p>单条语句、自带事务,不再是 existsById + deleteById(先非锁定读、再 find + remove)的三步。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(DELETE_BY_ID_JPQL)
    int deleteByZoneId(@Param("zoneId") Long zoneId);
}
