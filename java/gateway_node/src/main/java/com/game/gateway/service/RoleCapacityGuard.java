package com.game.gateway.service;

import com.game.gateway.config.HomeZoneProperties;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.repository.ZoneConfigRepository;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.time.LocalDateTime;
import java.util.EnumMap;
import java.util.HashMap;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.LongSupplier;
import java.util.function.Supplier;

/**
 * 本区角色上限守卫(2026-11-12,区服满员处理 §6,docs/design/zone-full-handling.md):本副本所在区
 * ({@code gateway.home-zone-ids})在区服目录里设了 {@code role_capacity}(大于 0)时,每
 * {@code role-capacity.check-interval-ms}(缺省 10 秒)问一次本区 login 一共建过多少角色(login 转问 data_service,
 * 按合服之后的最终区数,删角不减),不低于上限且创角开关还开着,就用比较并写关上 {@code create_role_closed}
 * ({@link ZoneConfigRepository#closeCreateRoleAtRoleCapacity})。
 *
 * <ul>
 *   <li><b>和手工关是同一个开关</b>:区服列表的「暂停创角」与不推荐、{@link ZonePolicyPublisher} 发给 login 的建角拦截
 *       都沿用,这里不另做第二份判断。</li>
 *   <li><b>只关不开</b>:放开是运维的决定(调高上限或置 0,再 {@code /create-role/open})。这是电平判断:角色数不低于上限时,
 *       手工打开也会在下一拍被关回去。</li>
 *   <li><b>不覆盖运维更新的设置</b>:关闭的条件里带着判断时读到的上限;运维在中间调了上限、置 0 或已手工关上,
 *       这次关闭一行都不改,下一拍按新值重判。</li>
 *   <li><b>软上限</b>:到线之后,至多一个检查周期、一次策略发布(5 秒)与 login 读策略(2 秒)内建出的角色会超出。
 *       硬上限要在建角事务里串行化一个按区计数行,一个区的建角吞吐被压成单行写,与水平扩展冲突;
 *       导量上限本来就是软目标。</li>
 *   <li><b>谁问</b>:每个区只由本区的网关副本问,问量随设了上限的区数线性增长,与网关副本总数无关;同区几个副本各问各的,
 *       比较并写保证只有一个关闭生效。没设上限的区不问。</li>
 *   <li><b>问不到不关</b>:login 或 data_service 不可用时只记这一区的 error,下一拍再问。问不到不等于满了;
 *       建角本身也要经过这两个服务,它们不在时本来就建不了角。</li>
 *   <li><b>线程</b>:跑在 Spring 的共享调度线程上。单区一次检查最坏占 {@code login.grpc.timeout-ms × (1 + retry)}
 *       (缺省 6 秒),只在 login 挂住不回时出现;连接被拒、没有通道都立即失败。</li>
 * </ul>
 * 只有调度线程调 {@link #check},字段不需要同步。
 */
@Component
public class RoleCapacityGuard {

    private static final Logger log = LoggerFactory.getLogger(RoleCapacityGuard.class);

    /** 失败日志的最小间隔,理由同 {@link ZonePolicyPublisher#ERROR_LOG_EVERY_MS}。持续多久看计数。 */
    static final long ERROR_LOG_EVERY_MS = 60_000L;

    /** 一拍里一个区的结果,也是计数 gateway_role_capacity_check_total 的 outcome 标签(封闭集合)。 */
    enum Outcome {
        /** 区服目录里没有本区。 */
        ZONE_MISSING("zone_missing"),
        /** 没设上限(0),不问。 */
        NO_CAP("no_cap"),
        /** 没到上限。 */
        BELOW("below"),
        /** 到了上限,但开关已经关着(手工关的,或之前已自动关上)。 */
        ALREADY_CLOSED("already_closed"),
        /** 这次关上了。 */
        CLOSED("closed"),
        /** 到了上限,但比较并写没命中:判断之后运维改了上限或开关。下一拍按新值重判。 */
        RACED("raced"),
        /** 问 login 或写库失败。 */
        ERROR("error");

        final String label;

        Outcome(String label) {
            this.label = label;
        }
    }

    private final ZoneConfigRepository zoneConfigRepo;
    private final HomeZoneProperties homeZones;
    private final LoginRpcClient loginRpc;
    private final MeterRegistry registry;
    private final Supplier<LocalDateTime> now;
    private final LongSupplier monotonicMs;
    private final Map<Outcome, Counter> outcomes = new EnumMap<>(Outcome.class);
    /** 每个设了上限的本区最近一次的读数。Micrometer 只弱引用 Gauge 的取值对象,这里持强引用。 */
    private final Map<Long, ZoneReading> readings = new HashMap<>();
    private long lastErrorLogMs = Long.MIN_VALUE;

    @Autowired
    public RoleCapacityGuard(ZoneConfigRepository zoneConfigRepo, HomeZoneProperties homeZones,
                             LoginRpcClient loginRpc, MeterRegistry registry) {
        this(zoneConfigRepo, homeZones, loginRpc, registry, LocalDateTime::now, () -> System.nanoTime() / 1_000_000L);
    }

    // 包级注入点:时钟与 AdminZoneController 同一来源(JVM 本地时间),测试可以固定;单调时钟只用于日志节流。
    RoleCapacityGuard(ZoneConfigRepository zoneConfigRepo, HomeZoneProperties homeZones, LoginRpcClient loginRpc,
                      MeterRegistry registry, Supplier<LocalDateTime> now, LongSupplier monotonicMs) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.homeZones = homeZones;
        this.loginRpc = loginRpc;
        this.registry = registry;
        this.now = now;
        this.monotonicMs = monotonicMs;
        for (Outcome outcome : Outcome.values()) {
            outcomes.put(outcome, Counter.builder("gateway.role_capacity.check")
                    .description("Role capacity checks per home zone and tick, by outcome")
                    .tag("outcome", outcome.label)
                    .register(registry));
        }
    }

    @Scheduled(fixedDelayString = "${role-capacity.check-interval-ms:10000}")
    public void check() {
        for (Long zoneId : homeZones.getHomeZoneIds()) {
            Outcome outcome;
            try {
                outcome = checkZone(zoneId);
            } catch (RuntimeException e) {
                outcome = Outcome.ERROR;
                long nowMs = monotonicMs.getAsLong();
                if (lastErrorLogMs == Long.MIN_VALUE || nowMs - lastErrorLogMs >= ERROR_LOG_EVERY_MS) {
                    lastErrorLogMs = nowMs;
                    log.error("Role capacity check failed for zone {} (role creation stays as it is); "
                            + "further failures are suppressed for {} ms, see gateway_role_capacity_check_total{outcome=\"error\"}",
                            zoneId, ERROR_LOG_EVERY_MS, e);
                }
            }
            outcomes.get(outcome).increment();
        }
    }

    /** 检查一个区:设了上限才问角色数,不低于上限且开关开着才关。 */
    Outcome checkZone(long zoneId) {
        Optional<ZoneConfig> row = zoneConfigRepo.findById(zoneId);
        if (row.isEmpty()) {
            clearCapacity(zoneId);
            return Outcome.ZONE_MISSING;
        }
        ZoneConfig zone = row.get();
        int roleCapacity = zone.getRoleCapacity();
        if (roleCapacity <= 0) {
            clearCapacity(zoneId);
            return Outcome.NO_CAP;
        }
        long roles = loginRpc.getZoneRoleCount(Math.toIntExact(zoneId));
        reading(zoneId).record(roles, roleCapacity);
        if (roles < roleCapacity) {
            return Outcome.BELOW;
        }
        if (zone.isCreateRoleClosed()) {
            return Outcome.ALREADY_CLOSED;
        }
        if (zoneConfigRepo.closeCreateRoleAtRoleCapacity(zoneId, roleCapacity, now.get()) == 0) {
            return Outcome.RACED;
        }
        log.warn("Zone {} reached its role capacity ({} roles >= {}); new character creation is paused. To allow more, "
                        + "raise role_capacity (PUT /admin/zones/{}/role-capacity) or set it to 0, then POST /admin/zones/{}/create-role/open",
                zoneId, roles, roleCapacity, zoneId, zoneId);
        return Outcome.CLOSED;
    }

    private ZoneReading reading(long zoneId) {
        return readings.computeIfAbsent(zoneId, id -> new ZoneReading(registry, id));
    }

    /** 上限取消或区服被删:导出的上限回 0(看板据此不再画上限线),角色数停在最后一次读数。从没设过上限的区不建序列。 */
    private void clearCapacity(long zoneId) {
        ZoneReading reading = readings.get(zoneId);
        if (reading != null) {
            reading.clearCapacity();
        }
    }

    /** 一个区最近一次的角色数与上限,导出成 gateway_zone_role_count / gateway_zone_role_capacity(标签 zone,基数 = 本副本的区数)。 */
    private static final class ZoneReading {
        private final AtomicLong roles = new AtomicLong();
        private final AtomicLong roleCapacity = new AtomicLong();

        ZoneReading(MeterRegistry registry, long zoneId) {
            String zone = Long.toString(zoneId);
            Gauge.builder("gateway.zone.role_count", roles, AtomicLong::get)
                    .description("Characters ever created in the zone (final zone after merges), as last read by the role capacity guard")
                    .tag("zone", zone)
                    .register(registry);
            Gauge.builder("gateway.zone.role_capacity", roleCapacity, AtomicLong::get)
                    .description("Role capacity of the zone (zone_config.role_capacity) at the last check")
                    .tag("zone", zone)
                    .register(registry);
        }

        void record(long roleCount, int capacity) {
            roles.set(roleCount);
            roleCapacity.set(capacity);
        }

        void clearCapacity() {
            roleCapacity.set(0);
        }
    }
}
