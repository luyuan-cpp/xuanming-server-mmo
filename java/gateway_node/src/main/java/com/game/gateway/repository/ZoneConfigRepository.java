package com.game.gateway.repository;

import com.game.gateway.entity.ZoneConfig;
import org.springframework.data.jpa.repository.Modifying;
import org.springframework.data.jpa.repository.Query;
import org.springframework.data.repository.Repository;
import org.springframework.data.repository.query.Param;
import org.springframework.transaction.annotation.Propagation;
import org.springframework.transaction.annotation.Transactional;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Optional;

/**
 * zone_config(选服目录)的读写。
 *
 * <h2>写路径:每条都是「按主键的单条语句」,只锁一条主键记录,且没有 S→X 升级(2026-09-21 死锁审计 #18 同类项)</h2>
 * zone_id 是业务赋值的主键(实体上没有生成策略),表上没有二级索引。写入只有四条:{@link #insertIfAbsent}(ODKU)、
 * {@link #updateSettings}、{@link #updateManualStatus}、{@link #deleteByZoneId}。每条语句在 InnoDB 里最多取
 * <b>一把</b>行锁 —— 主键记录上的 X(记录不在时是一把间隙锁,间隙锁从不等待)—— 而且是它所在事务的第一把锁;
 * 调用方事务里其后只有非锁定的一致性读回读。一个事务「等锁时手里什么都没有、拿到锁之后再不等」,就不可能出现在
 * 任何等待环上,所以这四条之间、以及与任何别的写者之间都只会排队。update 与改维护状态在写之前先用
 * {@link #lockZoneIdForUpdate} 按主键加锁判存在:那一把锁就是事务的第一把,随后的 UPDATE 改同一条已持 X 的记录,
 * 不再等锁,上面的结论不变。
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
     * {@link #insertIfAbsentColumns} 的语句。抽成常量,是为了让真库锁序回归用例执行与生产<b>同一份</b>文本。
     *
     * <p>语义:不存在则插入整行(created_at = updated_at = :now);<b>已存在则一列都不改</b> —— 重复键分支是不改值的
     * 自引用 {@code zone_id = zone_id},只为让重复键检查走 ODKU 的 X 锁(锁序论证见 {@link #insertIfAbsent})。
     *
     * <p>2026-11-05 之前这里是整行覆盖(与旧 create 的 {@code save()}/merge 一致):POST 一个已存在的 zone_id
     * 会把全部业务列改成请求体的值,请求体漏的字段又被实体的基本类型初值顶上,维护中的区服因此被改成 OPEN
     * (第一轮数据审计 §4 第 18 条的 POST 变种)。现在「新建」绝不改写已有的行;改已有的行只能走 PUT 或 /maintenance、/open。
     */
    String INSERT_IF_ABSENT_SQL = "INSERT INTO zone_config (zone_id, name, manual_status, capacity, maintenance_msg,"
            + " open_time, recommended, sort_order, created_at, updated_at)"
            + " VALUES (:zoneId, :name, :manualStatus, :capacity, :maintenanceMsg, :openTime,"
            + " :recommended, :sortOrder, :now, :now)"
            + " ON DUPLICATE KEY UPDATE zone_id = zone_id";

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

    /** {@link #lockZoneIdForUpdate} 的语句:按主键的加锁点查,只取主键列。 */
    String LOCK_FOR_UPDATE_SQL = "SELECT zone_id FROM zone_config WHERE zone_id = :zoneId FOR UPDATE";

    /** 全部区服。普通一致性读,不加锁。 */
    List<ZoneConfig> findAll();

    /** 按主键点查。普通一致性读,不加锁。 */
    Optional<ZoneConfig> findById(Long zoneId);

    /**
     * 按主键加锁判存在:行在则取它的 X 并持到事务结束,返回只含这个 zone_id 的列表;行不在返回空列表。
     *
     * <p>为什么要有它(2026-11-05,mmorpg 迁 TiDB,docs/design/no-single-node-horizontal-scaling-20261001.md §6.4):
     * 「改已存在的区服、不在就回 404」若靠 UPDATE 之后的回读判定存在性,只在 MySQL 的 REPEATABLE READ 下成立 ——
     * 靠 UPDATE 没命中时留下的间隙锁挡住并发插入。READ COMMITTED 与 TiDB 都没有这把间隙锁,并发 create 能插在
     * UPDATE 与回读之间,回读看见那一行就回 200,而 UPDATE 其实一行没改。先加锁点查、再决定执行不执行 UPDATE,
     * 存在性就不再依赖隔离级别。
     *
     * <p>取锁口径与类注释一致:它是调用方事务里的第一把锁,随后的 UPDATE 改的是同一条已持 X 的记录,不再等锁。
     * 只能在调用方的事务里调用:自动提交下加锁读读完就释放锁,判了也白判,所以声明为 MANDATORY,没有事务直接抛错。
     */
    @Transactional(propagation = Propagation.MANDATORY)
    @Query(value = LOCK_FOR_UPDATE_SQL, nativeQuery = true)
    List<Long> lockZoneIdForUpdate(@Param("zoneId") Long zoneId);

    /**
     * 区服不存在则按 {@code zone} 插入整行,已存在则什么也不改:见 {@link #INSERT_IF_ABSENT_SQL}。{@code zone} 必须是
     * 校验过的完整设置({@link com.game.gateway.dto.ZoneSettingsRequest#toZoneConfig}):实体字段是带初值的基本类型,
     * 没校验的对象会把初值当真值插进去;name 列 NOT NULL,库的 sql_mode 若不含 STRICT,NULL 会被静默改成 ''。
     *
     * <h3>为什么 ODKU 只会排队</h3>
     * ODKU 在主键重复检查时对已存在 / 删除标记的记录直接取 <b>X</b>(记录锁,不带间隙),其后「插入到删除标记记录上」
     * 或走 UPDATE 分支要的也是同一把 X —— 没有「先拿 S、再升级」这一步;UPDATE 分支改不改值不影响取锁。X 与 X 互斥,
     * 同键的并发者只能一个一个拿到:先到者插入并提交,后到者拿到锁时看见的是活行,走(不改值的)UPDATE 分支。
     * 键完全不存在时直接插入新记录(隐式锁),只在插入意向上等别人的间隙锁;而本仓库对这张表唯一会留下间隙锁的写者是
     * 「更新 / 删除一个不存在的 zone_id」,它拿到那把间隙锁之后不再等任何锁,构不成环。
     *
     * <h3>剩下去不掉的固有情形</h3>
     * 同键并发写入且<b>先到者回滚</b>,或 purge 恰在排队期间清掉删除标记记录:排队者挂在那条记录上的锁被 InnoDB
     * 继承成后继记录上的间隙锁,两个排队者的插入意向互相挡住 → 1213。这与语句写法无关,由调用方
     * (AdminZoneController#create)做有上限的整事务重试兜住,收敛论证见那里。
     *
     * <p>返回值是驱动报告的行数,调用方不要拿它区分插入 / 已存在(Connector/J 默认报 found rows,不改值的 UPDATE
     * 分支也报 1)。要知道结果,在同一事务里回读,再与请求逐列比较。
     */
    default int insertIfAbsent(ZoneConfig zone, LocalDateTime now) {
        return insertIfAbsentColumns(zone.getZoneId(), zone.getName(), zone.getManualStatus(), zone.getCapacity(),
                zone.getMaintenanceMsg(), zone.getOpenTime(), zone.isRecommended(), zone.getSortOrder(), now);
    }

    /**
     * {@link #insertIfAbsent} 的列级形式。调用方用 {@link #insertIfAbsent},这里只负责绑定。
     *
     * <p>native 语句绕过持久化上下文,所以先 flush 再 clear,免得同一事务里随后读到缓存里的旧实体。
     */
    @Modifying(flushAutomatically = true, clearAutomatically = true)
    @Transactional
    @Query(value = INSERT_IF_ABSENT_SQL, nativeQuery = true)
    int insertIfAbsentColumns(@Param("zoneId") Long zoneId, @Param("name") String name,
                              @Param("manualStatus") int manualStatus, @Param("capacity") int capacity,
                              @Param("maintenanceMsg") String maintenanceMsg, @Param("openTime") LocalDateTime openTime,
                              @Param("recommended") boolean recommended, @Param("sortOrder") int sortOrder,
                              @Param("now") LocalDateTime now);

    /**
     * 覆盖已存在区服的全部业务列(不含 created_at),刷新 updated_at;区服不存在时什么也不写,<b>不会</b>插入。
     * {@code values} 必须是校验过的完整设置(理由同 {@link #insertIfAbsent}):PUT 是完整资源替换,请求体漏字段由
     * 控制器在接缝处回 400,不能让实体初值替它补上。
     *
     * <p>单条按主键的 UPDATE:记录在则取它的 X,不在则只取一把间隙锁;之后调用方只做非锁定回读。
     * 返回值同 {@link #insertIfAbsent},不要拿它判断存在性(found rows / affected rows 取决于连接参数),在同一事务里回读。
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
