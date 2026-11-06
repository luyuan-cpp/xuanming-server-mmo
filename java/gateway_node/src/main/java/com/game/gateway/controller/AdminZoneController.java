package com.game.gateway.controller;

import com.game.gateway.dto.ManualZoneStatus;
import com.game.gateway.dto.ZoneSettingsRequest;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;
import org.springframework.web.bind.annotation.*;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Objects;
import java.util.Optional;
import java.util.function.Supplier;

/**
 * 区服目录的运维接口。
 *
 * <h2>写路径(2026-09-21 死锁审计 #18 同类项)</h2>
 * 每个写接口都是「一条按主键的写语句 + 同一事务内的非锁定回读」,只锁一条主键记录,取锁论证见
 * {@link ZoneConfigRepository}。旧写法走 JPA 的 {@code save()}(merge → 普通 INSERT)会在主键重复检查上
 * S→X 互等,且 findById + save 在行被删的窗口里会把区服重新 INSERT 回来;现在都没有了。
 *
 * <h2>完整资源语义(2026-11-05,第一轮数据审计 §4 第 18 条)</h2>
 * create / update 的请求体是 {@link ZoneSettingsRequest}:每个业务字段都必须给,缺一个或取值越界就回 400、不碰库。
 * 之前两者直接绑实体,实体字段是带初值的基本类型,漏传 manualStatus 就是 0 = OPEN,一次只想改名的 PUT 会把维护中的
 * 区服改成开放,容量、推荐、排序、开服时间、维护文案一并被初值覆盖;create 撞上已存在的 zone_id 也走同样的整行覆盖。
 * 现在 create 只插不改(已存在且内容不同回 409),局部修改走 {@code /maintenance}、{@code /open}。
 *
 * <p>返回形状:200 + ZoneConfig JSON / 204 / 404 不变;新增 400(请求体不完整或不合法,原因记 WARN 日志)与
 * 409(create 撞上已存在且内容不同的区服,响应体是库里那一行)。时间戳上的有意变化写在各接口注释里。
 */
@RestController
@RequestMapping("/admin/zones")
public class AdminZoneController {

    private static final Logger log = LoggerFactory.getLogger(AdminZoneController.class);

    private final ZoneConfigRepository zoneConfigRepo;
    private final TransactionOperations tx;
    private final Supplier<LocalDateTime> now;

    @Autowired
    public AdminZoneController(ZoneConfigRepository zoneConfigRepo, TransactionOperations tx) {
        this(zoneConfigRepo, tx, LocalDateTime::now);
    }

    // 包级时钟注入点:与 ZoneConfig 的 @PrePersist / @PreUpdate 同一时钟源(JVM 本地时间),测试可以固定时间。
    AdminZoneController(ZoneConfigRepository zoneConfigRepo, TransactionOperations tx, Supplier<LocalDateTime> now) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.tx = tx;
        this.now = now;
    }

    @GetMapping
    public List<ZoneConfig> listAll() {
        return zoneConfigRepo.findAll();
    }

    @GetMapping("/{zoneId}")
    public ResponseEntity<ZoneConfig> getById(@PathVariable Long zoneId) {
        return zoneConfigRepo.findById(zoneId)
                .map(ResponseEntity::ok)
                .orElse(ResponseEntity.notFound().build());
    }

    /**
     * 创建区服:zone_id 不存在则按请求体插入整行;<b>已存在则一列都不改</b>。回读到的那一行与请求体逐列一致回 200
     * (同一份请求重放是幂等的),不一致回 409 + 库里那一行 —— 要改已有的区服走 PUT 或 /maintenance、/open。
     * 请求体必须给全每个业务字段(见 {@link ZoneSettingsRequest#problem}),否则 400、不碰库。
     *
     * <p>写入用 {@link ZoneConfigRepository#insertIfAbsent}(ODKU,重复键分支不改值):主键重复检查直接取 X,同键并发的
     * create 只排队,不再有普通 INSERT 的 S→X 互等。写入与回读在同一事务里:语句持有该行的 X 直到提交,其间并发的
     * delete 删不掉它;本事务自己的插入对自己的读一律可见,已存在的那一行在本事务拿到锁之前已经提交,回读也看得见,
     * 所以回读必然命中。整事务包在 {@link InnoDbDeadlockRetry#execute} 里,只兜「同键并发写入 + 先到者回滚 / purge
     * 恰在排队窗口」这类 InnoDB 固有的 1213,收敛论证见那里。
     *
     * <p>「插入了还是本来就在」不看驱动报的行数(found rows 下两者都报 1),只看回读与请求是否逐列一致:一致说明库里已经是
     * 请求要的样子,是谁写的都不必再改;不一致说明那一行不是这次写的。这条判据与隔离级别无关,换成 RC 也成立。
     *
     * <p>相对 2026-11-05 之前的有意变化(第一轮数据审计 §4 第 18 条的 POST 变种):已存在的 zone_id 不再被整行覆盖,
     * updated_at 也不再刷新;缺任一业务字段回 400(之前只查 zone_id 与 name,其余字段落成实体初值,
     * manualStatus 缺省就是 OPEN)。
     */
    @PostMapping
    public ResponseEntity<ZoneConfig> create(@RequestBody ZoneSettingsRequest body) {
        String problem = body == null ? "request body is missing" : body.problem(null);
        if (problem != null) {
            return rejected("create", null, problem);
        }
        long zoneId = body.getZoneId();
        ZoneConfig requested = body.toZoneConfig(zoneId);
        LocalDateTime at = now.get();
        ZoneConfig stored = InnoDbDeadlockRetry.execute(tx, "zone create", zoneId, () -> {
            zoneConfigRepo.insertIfAbsent(requested, at);
            return zoneConfigRepo.findById(zoneId)
                    .orElseThrow(() -> new IllegalStateException(
                            "zone_config row invisible after insert-if-absent in the same transaction, zone_id=" + zoneId));
        });
        if (!sameSettings(requested, stored)) {
            log.warn("zone create refused: zone {} already exists with different settings; use PUT to replace it", zoneId);
            return ResponseEntity.status(HttpStatus.CONFLICT).body(stored);
        }
        return ResponseEntity.ok(stored);
    }

    /**
     * 以请求体<b>整行替换</b>已存在区服的业务列(完整资源语义:每个业务字段都必须给,见 {@link ZoneSettingsRequest#problem}),
     * 区服不存在回 404,<b>绝不插入</b>。请求体带 zoneId 时必须与路径一致。
     *
     * <p>存在性由同一事务里的回读判定,不看 UPDATE 的行数:Connector/J 默认报 found rows,但连接参数一旦改成
     * useAffectedRows=true,「值没变」的 UPDATE 会报 0 行,拿行数判 404 就错了。
     *
     * <h3>回读与 UPDATE 结论一致的前提:REPEATABLE READ,且事务里没有更早的一致性读</h3>
     * 两个方向的论证不对称,写清楚才能在换隔离级别时看出破在哪:
     * <ul>
     *   <li><b>行在 → 200</b>:UPDATE 拿到该主键记录的 X 并持到提交,并发的 delete 删不掉它,回读必然看得见。
     *       这一条与隔离级别无关,RC 下同样成立。</li>
     *   <li><b>行不在 → 404</b>:<b>只在 RR 下成立</b>,靠的是间隙锁。RR 下 UPDATE 在聚簇索引上定位到该键之后的
     *       第一条记录,取的是 next-key 锁 —— 含该键所在的那段间隙,并发 INSERT 的插入意向锁被挡住;手册说的
     *       「唯一索引等值查找只锁记录、不锁前面的间隙」是<b>命中</b>时的优化,没命中就没有记录可锁,间隙锁留下。
     *       RC 明确关掉了这一层:手册 innodb-transaction-isolation-levels「READ COMMITTED」原话是,
     *       对加锁读 / UPDATE / DELETE「InnoDB locks only index records, not the gaps before them」,
     *       间隙锁只留给外键检查与唯一键重复检查。于是 RC 下并发 create 能在 UPDATE 与回读之间插入并提交,
     *       而 RC 的一致性读每条语句都取新读视图,回读会看见那一行 —— 「UPDATE 一行没改,却回 200」。</li>
     * </ul>
     * 本服务没有在 datasource / @Transactional 上覆盖隔离级别,用的是 MySQL 8 的服务端默认 RR
     * (application.yaml 的 spring.datasource.url 里没有 sessionVariables=transaction_isolation=...)。
     * 要改成 RC(例如为迁 TiDB 统一口径,见 docs/design/global-data-layer-tidb-decision.md),这里必须一起改成
     * <b>加锁</b>回读(SELECT ... FOR UPDATE)或「按主键 FOR UPDATE 判存在 → 再 UPDATE」,不能只换隔离级别。
     * TiDB 没有间隙锁,同一条论证在 TiDB 上本来就不成立,迁库时这段必须重做。
     *
     * <p>另一半前提是语句顺序:RR 的一致性读视图在事务的<b>第一次一致性读</b>时建立。现在这个事务里 UPDATE 在前、
     * 回读在后,所以读视图建立在 UPDATE 之后,回读看得到 UPDATE 自己的效果和它之前已提交的一切。若日后在
     * UPDATE 前面插一句一致性读,读视图就会提前到那一刻,「UPDATE 改到了一行、回读却看不见」的组合就出现了 ——
     * 这个事务里不要在 UPDATE 之前加读。
     *
     * <p>不重试:事务里只有这一条加锁语句,它等锁时手里什么都没有、拿到锁之后不再等,不可能成为死锁牺牲者。
     *
     * <p>有意变化:缺任一业务字段、取值越界、请求体 zoneId 与路径不一致都回 400 且不碰库(2026-11-05,第一轮数据审计
     * §4 第 18 条:之前只查 name,只想改名的 PUT 会把 manualStatus 等字段写成实体初值,维护中的区服被改成 OPEN);
     * 每次调用都刷新 updated_at。
     */
    @PutMapping("/{zoneId}")
    public ResponseEntity<ZoneConfig> update(@PathVariable Long zoneId, @RequestBody ZoneSettingsRequest body) {
        String problem = body == null ? "request body is missing" : body.problem(zoneId);
        if (problem != null) {
            return rejected("update", zoneId, problem);
        }
        ZoneConfig values = body.toZoneConfig(zoneId);
        LocalDateTime at = now.get();
        return okOrNotFound(tx.execute(status -> {
            zoneConfigRepo.updateSettings(zoneId, values, at);
            return zoneConfigRepo.findById(zoneId);
        }));
    }

    /**
     * 删除区服:存在则删并回 204,不存在回 404。单条按主键的 DELETE,行数就是真正删掉的行数,直接用它区分。
     * 不重试,理由同 {@link #update}。
     */
    @DeleteMapping("/{zoneId}")
    public ResponseEntity<Void> delete(@PathVariable Long zoneId) {
        if (zoneConfigRepo.deleteByZoneId(zoneId) > 0) {
            return ResponseEntity.noContent().build();
        }
        return ResponseEntity.notFound().build();
    }

    /**
     * Quick action: set a zone to maintenance.
     * POST /admin/zones/1/maintenance
     * Body: { "maintenanceMsg": "Estimated recovery at 14:00" }
     *
     * <p>只改 manual_status 与(请求里给了的)维护文案;没给文案则保留原文案。旧写法 findById + save 会把其余列
     * 用读到的旧快照整行写回,与并发的 update 交错时丢失对方的修改;现在其余列不动。存在性判定与不重试的理由同
     * {@link #update}。
     */
    @PostMapping("/{zoneId}/maintenance")
    public ResponseEntity<ZoneConfig> setMaintenance(@PathVariable Long zoneId,
                                                     @RequestBody(required = false) MaintenanceRequest req) {
        String keepMessageIfNull = (req != null) ? req.maintenanceMsg() : null;
        return setManualStatus(zoneId, ManualZoneStatus.MAINTENANCE, keepMessageIfNull);
    }

    /**
     * Quick action: set a zone back to open.
     * POST /admin/zones/1/open
     *
     * <p>manual_status 置 OPEN 并清空维护文案,其余列不动。口径同 {@link #setMaintenance}。
     */
    @PostMapping("/{zoneId}/open")
    public ResponseEntity<ZoneConfig> setOpen(@PathVariable Long zoneId) {
        return setManualStatus(zoneId, ManualZoneStatus.OPEN, "");
    }

    private ResponseEntity<ZoneConfig> setManualStatus(Long zoneId, ManualZoneStatus status, String maintenanceMsg) {
        LocalDateTime at = now.get();
        return okOrNotFound(tx.execute(txStatus -> {
            zoneConfigRepo.updateManualStatus(zoneId, status.getCode(), maintenanceMsg, at);
            return zoneConfigRepo.findById(zoneId);
        }));
    }

    private static ResponseEntity<ZoneConfig> okOrNotFound(Optional<ZoneConfig> zone) {
        return zone.map(ResponseEntity::ok).orElseGet(() -> ResponseEntity.notFound().build());
    }

    /** 请求体不完整或不合法:400、不碰库;原因只含字段名,不含请求里的值。 */
    private static ResponseEntity<ZoneConfig> rejected(String action, Long zoneId, String problem) {
        log.warn("zone {} rejected: zone={} reason={}", action, zoneId, problem);
        return ResponseEntity.badRequest().build();
    }

    /** 两行的业务列是否逐列相同(不比 created_at / updated_at)。create 用它判「库里已是请求要的样子」。 */
    private static boolean sameSettings(ZoneConfig requested, ZoneConfig stored) {
        return Objects.equals(requested.getName(), stored.getName())
                && requested.getManualStatus() == stored.getManualStatus()
                && requested.getCapacity() == stored.getCapacity()
                && Objects.equals(requested.getMaintenanceMsg(), stored.getMaintenanceMsg())
                && Objects.equals(requested.getOpenTime(), stored.getOpenTime())
                && requested.isRecommended() == stored.isRecommended()
                && requested.getSortOrder() == stored.getSortOrder();
    }

    public record MaintenanceRequest(String maintenanceMsg) {}
}
