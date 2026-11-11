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
 * {@link ZoneConfigRepository}。update 与改维护状态在写之前先按主键加锁判存在(2026-11-05,mmorpg 迁 TiDB),
 * 理由见 {@link #update}。旧写法走 JPA 的 {@code save()}(merge → 普通 INSERT)会在主键重复检查上
 * S→X 互等,且 findById + save 在行被删的窗口里会把区服重新 INSERT 回来;现在都没有了。
 *
 * <h2>完整资源语义(2026-11-05,第一轮数据审计 §4 第 18 条)</h2>
 * create / update 的请求体是 {@link ZoneSettingsRequest}:每个业务字段都必须给,缺一个或取值越界就回 400、不碰库。
 * 之前两者直接绑实体,实体字段是带初值的基本类型,漏传 manualStatus 就是 0 = OPEN,一次只想改名的 PUT 会把维护中的
 * 区服改成开放,容量、推荐、排序、开服时间、维护文案一并被初值覆盖;create 撞上已存在的 zone_id 也走同样的整行覆盖。
 * 现在 create 只插不改(已存在且内容不同回 409),局部修改走 {@code /maintenance}、{@code /open}。
 * 「暂停创建新角色」(create_role_closed,2026-11-10)不在整行设置里:它是运营开关,只经 {@code /create-role/close}、
 * {@code /create-role/open} 改,POST 新建时取库缺省(开着),PUT 整行替换不碰它。
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
     * <p>存在性由同一事务里、UPDATE 之前的<b>加锁点查</b>判定({@link ZoneConfigRepository#lockZoneIdForUpdate}),
     * 不看 UPDATE 的行数:Connector/J 默认报 found rows,但连接参数一旦改成 useAffectedRows=true,「值没变」的
     * UPDATE 会报 0 行,拿行数判 404 就错了。
     *
     * <h3>为什么先加锁点查(2026-11-05,mmorpg 迁 TiDB)</h3>
     * <ul>
     *   <li><b>行在 → 200</b>:点查拿到该主键记录的 X 并持到提交,并发的 delete 删不掉它;随后的 UPDATE 改的就是这一行,
     *       回读必然看得见本事务的修改。</li>
     *   <li><b>行不在 → 404</b>:直接回 404,不执行 UPDATE。并发 create 即便紧接着插进来,本次请求也是在「区服还不存在」
     *       的那一刻做的判定,没有修改任何东西,回 404 是对的。</li>
     * </ul>
     * 两条都不依赖隔离级别,MySQL 的 RR、RC 与 TiDB(悲观事务、RC)上同样成立。之前的写法是「先 UPDATE、再非锁定
     * 回读」,「行不在 → 404」只在 MySQL 的 RR 下成立:靠 UPDATE 没命中时留下的间隙锁挡住并发插入。RC 关掉了这层
     * 间隙锁,TiDB 本来就没有,于是并发 create 能插在 UPDATE 与回读之间,出现「UPDATE 一行没改,却回 200」。
     *
     * <p>回读看得到本事务的 UPDATE:加锁点查不建立一致性读视图;RR 下读视图建立在 UPDATE 之后的回读那一刻,
     * RC 与 TiDB 每条语句取新视图,三种情形都看得见自己的修改。
     *
     * <p>不重试:事务里第一把锁是加锁点查,它等锁时手里什么都没有;拿到之后 UPDATE 改同一条已持锁的记录,不再等锁,
     * 不可能成为死锁牺牲者。
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
        return okOrNotFound(writeExistingZone(zoneId, () -> zoneConfigRepo.updateSettings(zoneId, values, at)));
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
     * Body: { "maintenance_msg": "Estimated recovery at 14:00" }
     *
     * <p>JSON 键是 snake_case(application.yaml 的 {@code spring.jackson.property-naming-strategy: SNAKE_CASE})。
     * 写成驼峰 {@code maintenanceMsg} 会被 Jackson 静默忽略,等于没给文案、保留原文案。
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

    /**
     * 暂停本区创建新角色(2026-11-10,区服满员处理)。
     * POST /admin/zones/1/create-role/close
     *
     * <p>只拦新建:账号在本区已有的角色照常登录进场,login 只拒绝 CreatePlayer(tip kLoginZoneCreateRoleClosed)。
     * 区服列表随 zone_config 行缓存(1 秒)立即显示 create_role_closed、不再推荐这个区;本区网关的 ZonePolicyPublisher
     * 下一拍(至多 5 秒)写到 etcd,本区 login 至多再晚 2 秒读到。只改这一列,其余列不动;存在性判定与不重试的理由同
     * {@link #update}。什么时候该关:按本区角色总量(而不是某一时刻的在线人数)判断,在线高峰由登录排队承担。
     */
    @PostMapping("/{zoneId}/create-role/close")
    public ResponseEntity<ZoneConfig> closeRoleCreation(@PathVariable Long zoneId) {
        return setCreateRoleClosed(zoneId, true);
    }

    /**
     * 恢复本区创建新角色。POST /admin/zones/1/create-role/open。口径同 {@link #closeRoleCreation}。
     */
    @PostMapping("/{zoneId}/create-role/open")
    public ResponseEntity<ZoneConfig> openRoleCreation(@PathVariable Long zoneId) {
        return setCreateRoleClosed(zoneId, false);
    }

    private ResponseEntity<ZoneConfig> setCreateRoleClosed(Long zoneId, boolean closed) {
        LocalDateTime at = now.get();
        return okOrNotFound(writeExistingZone(zoneId,
                () -> zoneConfigRepo.updateCreateRoleClosed(zoneId, closed, at)));
    }

    private ResponseEntity<ZoneConfig> setManualStatus(Long zoneId, ManualZoneStatus status, String maintenanceMsg) {
        LocalDateTime at = now.get();
        return okOrNotFound(writeExistingZone(zoneId,
                () -> zoneConfigRepo.updateManualStatus(zoneId, status.getCode(), maintenanceMsg, at)));
    }

    /**
     * 改一个已存在的区服:同一事务里先按主键加锁判存在,在才执行 {@code write} 并回读;不在返回空(调用方回 404),
     * {@code write} 不执行。论证见 {@link #update}。
     */
    private Optional<ZoneConfig> writeExistingZone(Long zoneId, Runnable write) {
        return tx.execute(txStatus -> {
            if (zoneConfigRepo.lockZoneIdForUpdate(zoneId).isEmpty()) {
                return Optional.empty();
            }
            write.run();
            return zoneConfigRepo.findById(zoneId);
        });
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
