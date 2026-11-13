package com.game.gateway.controller;

import com.game.gateway.dto.FieldViolation;
import com.game.gateway.dto.ManualZoneStatus;
import com.game.gateway.dto.ZoneSettingsRequest;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpHeaders;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.transaction.support.TransactionOperations;
import org.springframework.web.bind.annotation.*;

import java.time.LocalDateTime;
import java.util.List;
import java.util.Objects;
import java.util.Optional;
import java.util.OptionalLong;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.LongSupplier;
import java.util.function.Supplier;

/**
 * 区服目录的运维接口。
 *
 * <h2>写路径(2026-09-21 死锁审计 #18 同类项)</h2>
 * 每个写接口都是「一条按主键的写语句 + 同一事务内对同一行的回读」,只锁一条主键记录,取锁论证见
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
 * {@code /create-role/open} 改,POST 新建时取库缺省(开着),PUT 整行替换不碰它。本区角色上限(role_capacity,2026-11-12)
 * 同理,只经 {@code PUT /role-capacity} 改。
 *
 * <h2>乐观并发(2026-11-12,数据审计遗留)</h2>
 * 之前两个运维先后 GET 再 PUT,后写的会整行覆盖前写的。现在按 HTTP 的条件请求做(RFC 9110 §13、RFC 6585 §3):
 * <ul>
 *   <li>{@code GET /admin/zones/{id}} 回 {@code ETag}(这一行的版本,见 ZoneConfig#getVersion)。带 {@code If-None-Match}
 *       且没变时回 304,由 Spring 按 ETag 自动处理。列表 {@code GET /admin/zones} 不带 ETag:要改哪一区先单独 GET 它。</li>
 *   <li>整行 {@code PUT} 必须带 {@code If-Match}(GET 拿到的 ETag 原值):没带或是 {@code *} 回 428,与当前版本不符回 412,
 *       都不写。比较与写入在同一条条件 UPDATE 里(ZoneConfigRepository#updateSettings),没有「先读后比再写」的窗口。
 *       区服不在时回 404 优先于 412(RFC 9110 §13.2.1)。</li>
 *   <li>{@code /maintenance}、{@code /open}、{@code /create-role/close|open}、{@code /role-capacity} 不要求 If-Match,带了也不看。
 *       它们是命令:调用方给的是这几列的目标值,不是改过的整行,不会把读到的旧值写回去;两个命令先后到达时,行锁让它们排队,
 *       后到的就是运维最后的意图,这正是开关该有的语义;紧急置维护也不该先 GET 一次、再因为 412 重来。它们照样让版本加一,
 *       所以基于更早 GET 的整行 PUT 会回 412,不会把刚置的维护状态悄悄改回开放。本区网关按角色上限关创角(RoleCapacityGuard)
 *       同理。DELETE 也不带条件:它不写任何值。</li>
 * </ul>
 *
 * <p>返回形状:200 + ZoneConfig JSON / 204 / 404 不变;409(create 撞上已存在且内容不同的区服,响应体是库里那一行)不变。
 * 400、412、428 的响应体是 problem details(application/problem+json),带字段名与原因码,形状与原因码一览见
 * {@link AdminProblems};2026-11-12 之前 400 不带响应体,原因只在 WARN 日志里。时间戳上的有意变化写在各接口注释里。
 */
@RestController
@RequestMapping("/admin/zones")
public class AdminZoneController {

    private static final Logger log = LoggerFactory.getLogger(AdminZoneController.class);

    /** 新行起始版本的上界(不含)。取 2^53,与 RowIds 同一上界,之上给每行留足自增的空间。 */
    static final long NEW_ROW_VERSION_BOUND = 1L << 53;

    /** create / update 没给请求体。 */
    private static final FieldViolation MISSING_BODY = FieldViolation.missing(null, "request body is missing");

    private final ZoneConfigRepository zoneConfigRepo;
    private final TransactionOperations tx;
    private final Supplier<LocalDateTime> now;
    private final LongSupplier newRowVersions;

    @Autowired
    public AdminZoneController(ZoneConfigRepository zoneConfigRepo, TransactionOperations tx) {
        this(zoneConfigRepo, tx, LocalDateTime::now,
                () -> ThreadLocalRandom.current().nextLong(1, NEW_ROW_VERSION_BOUND));
    }

    // 包级注入点:时钟与 ZoneConfig 的 @PrePersist / @PreUpdate 同一来源(JVM 本地时间);新行起始版本的来源见 create。
    // 测试可以都固定。
    AdminZoneController(ZoneConfigRepository zoneConfigRepo, TransactionOperations tx, Supplier<LocalDateTime> now,
                        LongSupplier newRowVersions) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.tx = tx;
        this.now = now;
        this.newRowVersions = newRowVersions;
    }

    @GetMapping
    public List<ZoneConfig> listAll() {
        return zoneConfigRepo.findAll();
    }

    /** 一个区服的整行,响应头 ETag 是它的当前版本(整行 PUT 的 If-Match 用它,见类注释「乐观并发」)。 */
    @GetMapping("/{zoneId}")
    public ResponseEntity<ZoneConfig> getById(@PathVariable Long zoneId) {
        return zoneConfigRepo.findById(zoneId)
                .map(zone -> ResponseEntity.ok().eTag(ZoneEntityTag.of(zone.getVersion())).body(zone))
                .orElse(ResponseEntity.notFound().build());
    }

    /**
     * 创建区服:zone_id 不存在则按请求体插入整行;<b>已存在则一列都不改</b>。回读到的那一行与请求体逐列一致回 200
     * (同一份请求重放是幂等的),不一致回 409 + 库里那一行 —— 要改已有的区服走 PUT 或 /maintenance、/open。
     * 请求体必须给全每个业务字段(见 {@link ZoneSettingsRequest#violation}),否则 400、不碰库。
     *
     * <p>写入用 {@link ZoneConfigRepository#insertIfAbsent}(ODKU,重复键分支不改值):主键重复检查直接取 X,同键并发的
     * create 只排队,不再有普通 INSERT 的 S→X 互等。写入与回读在同一事务里:语句持有该行的 X 直到提交,其间并发的
     * delete 删不掉它。回读用加锁读 {@link ZoneConfigRepository#findByIdForUpdate}(2026-11-12):加锁读在 MySQL 与 TiDB 上
     * 都读最新的已提交版本加本事务自己的修改,不看事务快照,所以插入分支读得到自己刚插的行,已存在分支读得到别人此前提交的行,
     * 回读必然命中,与隔离级别无关;锁本事务已经拿着,不会再等。之前是普通一致性读,「已存在」分支里 ODKU 一列不改,
     * 回读全靠快照晚于那一行的提交:MySQL 的 RR 与 RC、TiDB 的 RC 成立,TiDB 的 RR(快照建在事务开始时)就不成立,
     * 而 zone_config 所在的 mmorpg 库缺省就在 TiDB 上(细节见 findByIdForUpdate)。
     * 整事务包在 {@link InnoDbDeadlockRetry#execute} 里,只兜「同键并发写入 + 先到者回滚 / purge
     * 恰在排队窗口」这类 InnoDB 固有的 1213,收敛论证见那里。
     *
     * <p>「插入了还是本来就在」不看驱动报的行数(found rows 下两者都报 1),只看回读与请求是否逐列一致:一致说明库里已经是
     * 请求要的样子,是谁写的都不必再改;不一致说明那一行不是这次写的。回读读的是最新版本(上一段),所以这条判据也不依赖隔离级别。
     *
     * <p>新行的起始版本取 [1, 2^53) 里的随机数,不从 1 起:zone_id 是业务赋值的,删掉再建同号区服之后,删之前 GET 到的 ETag
     * 不能恰好对上新行。从 1 起的话,新行被改过与旧行同样多次时版本号相同,拿着旧 ETag 的 PUT 会被当成基于最新版本,
     * 把旧设置整行写到新区服上。成功与 409 的响应都不带 ETag:要改先 GET。
     *
     * <p>相对 2026-11-05 之前的有意变化(第一轮数据审计 §4 第 18 条的 POST 变种):已存在的 zone_id 不再被整行覆盖,
     * updated_at 也不再刷新;缺任一业务字段回 400(之前只查 zone_id 与 name,其余字段落成实体初值,
     * manualStatus 缺省就是 OPEN)。
     */
    @PostMapping
    public ResponseEntity<ZoneConfig> create(@RequestBody(required = false) ZoneSettingsRequest body) {
        FieldViolation violation = body == null ? MISSING_BODY : body.violation(null);
        if (violation != null) {
            throw AdminProblems.badRequest(violation);
        }
        long zoneId = body.getZoneId();
        ZoneConfig requested = body.toZoneConfig(zoneId);
        requested.setVersion(newRowVersions.getAsLong());
        LocalDateTime at = now.get();
        ZoneConfig stored = InnoDbDeadlockRetry.execute(tx, "zone create", zoneId, () -> {
            zoneConfigRepo.insertIfAbsent(requested, at);
            return zoneConfigRepo.findByIdForUpdate(zoneId)
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
     * 以请求体<b>整行替换</b>已存在区服的业务列(完整资源语义:每个业务字段都必须给,见 {@link ZoneSettingsRequest#violation}),
     * 区服不存在回 404,<b>绝不插入</b>。请求体带 zoneId 时必须与路径一致。
     *
     * <h3>If-Match(2026-11-12)</h3>
     * 必须带 GET 拿到的 ETag:没带或是 {@code *} 回 428,不是一个实体标签回 400,都不碰库(解析规则见
     * {@link ZoneEntityTag#requiredVersion})。判定顺序:请求体 400 → If-Match 428 / 400 → 区服不在 404 → 版本不符 412 → 200。
     * 版本条件写在 UPDATE 的 WHERE 里({@link ZoneConfigRepository#updateSettings}),行数 0 即版本不符:存在性已由下面的
     * 加锁点查判过,而且这把锁持到提交,别人在点查与 UPDATE 之间改不了这一行。412 不写任何东西,调用方重新 GET、在新的整行上
     * 重做修改再 PUT。200 的响应不带 ETag(RFC 9110 §9.3.4:存下的表示与 PUT 的请求体不完全相同时不发校验器),
     * 要连着再改就再 GET 一次。
     *
     * <p>存在性由同一事务里、UPDATE 之前的<b>加锁点查</b>判定({@link ZoneConfigRepository#lockZoneIdForUpdate}),
     * 不靠 UPDATE 的行数:行数 0 也可能是版本不符,而点查已经分清了「不在」。
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
     * <p>回读看得到本事务的 UPDATE,与隔离级别无关:这一行刚被本事务改过(每条 UPDATE 都改 version,不会是「值没变」的空写),
     * MySQL 的一致性读总能看见本事务自己的修改;TiDB 的读先查本事务的写缓冲,命中就返回,而缓冲里是 UPDATE 按最新已提交版本
     * 算出的整行。所以这里的回读不需要像 create 那样用加锁读。
     *
     * <p>不重试:事务里第一把锁是加锁点查,它等锁时手里什么都没有;拿到之后 UPDATE 改同一条已持锁的记录,不再等锁,
     * 不可能成为死锁牺牲者。
     *
     * <p>有意变化:缺任一业务字段、取值越界、请求体 zoneId 与路径不一致都回 400 且不碰库(2026-11-05,第一轮数据审计
     * §4 第 18 条:之前只查 name,只想改名的 PUT 会把 manualStatus 等字段写成实体初值,维护中的区服被改成 OPEN);
     * 每次写入都刷新 updated_at;没带 If-Match 回 428(2026-11-12)。
     */
    @PutMapping("/{zoneId}")
    public ResponseEntity<ZoneConfig> update(@PathVariable Long zoneId,
                                             @RequestHeader(name = HttpHeaders.IF_MATCH, required = false) String ifMatch,
                                             @RequestBody(required = false) ZoneSettingsRequest body) {
        FieldViolation violation = body == null ? MISSING_BODY : body.violation(zoneId);
        if (violation != null) {
            throw AdminProblems.badRequest(violation);
        }
        OptionalLong expectedVersion = ZoneEntityTag.requiredVersion(ifMatch);
        ZoneConfig values = body.toZoneConfig(zoneId);
        LocalDateTime at = now.get();
        return okOrNotFound(writeExistingZone(zoneId, () -> {
            // 认不出的标签与任何版本都不相等(见 ZoneEntityTag),不必发 UPDATE。区服在不在已由加锁点查判过,这里只剩版本不符。
            if (expectedVersion.isEmpty()
                    || zoneConfigRepo.updateSettings(zoneId, values, expectedVersion.getAsLong(), at) == 0) {
                throw AdminProblems.versionMismatch();
            }
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

    /**
     * 设本区角色上限(2026-11-12,区服满员处理 §6)。PUT /admin/zones/1/role-capacity,Body: {@code {"role_capacity": 30000}},
     * 0 = 不设上限。JSON 键是 snake_case(理由同 {@link #setMaintenance})。缺字段或为负回 400、不碰库。
     *
     * <p>本区一共建过的角色数到了上限,本区网关的 RoleCapacityGuard 至多一个检查周期内关上创角开关(与手工
     * {@code /create-role/close} 是同一个开关,区服列表与 login 的生效路径相同)。上限是电平判断:角色数不低于上限时开关
     * 会被反复关上,所以要放开先调高上限或置 0,再 {@code /create-role/open}。只改这一列,其余列不动;
     * 存在性判定与不重试的理由同 {@link #update}。
     */
    @PutMapping("/{zoneId}/role-capacity")
    public ResponseEntity<ZoneConfig> setRoleCapacity(@PathVariable Long zoneId,
                                                      @RequestBody(required = false) RoleCapacityRequest req) {
        if (req == null || req.roleCapacity() == null) {
            throw AdminProblems.badRequest(
                    FieldViolation.missing("role_capacity", "role_capacity is required (0 = no cap)"));
        }
        if (req.roleCapacity() < 0) {
            throw AdminProblems.badRequest(FieldViolation.invalid("role_capacity", "role_capacity must be >= 0"));
        }
        int roleCapacity = req.roleCapacity();
        LocalDateTime at = now.get();
        return okOrNotFound(writeExistingZone(zoneId,
                () -> zoneConfigRepo.updateRoleCapacity(zoneId, roleCapacity, at)));
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
     * {@code write} 不执行。{@code write} 抛出的异常(例如整行 PUT 的 412)回滚整个事务并原样抛给调用方。论证见 {@link #update}。
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

    /** 两行的业务列是否逐列相同(不比 version / created_at / updated_at)。create 用它判「库里已是请求要的样子」。 */
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

    /** {@link #setRoleCapacity} 的请求体。用包装类型:漏传字段是 null(回 400),不会被当成 0(= 取消上限)。 */
    public record RoleCapacityRequest(Integer roleCapacity) {}
}
