package com.game.gateway.service;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.config.HomeZoneProperties;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.repository.ZoneConfigRepository;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.io.IOException;
import java.util.EnumMap;
import java.util.Map;
import java.util.Optional;
import java.util.function.LongSupplier;

/**
 * 区服策略发布器(2026-11-10,区服满员处理):把本副本所在区({@code gateway.home-zone-ids})在区服目录 zone_config 里的
 * 容量与「暂停创建新角色」写到 etcd 的 {@link #KEY_PREFIX}{@code <zoneId>},给本区 login 读
 * (go/login/internal/logic/pkg/zonepolicy:排队的区容量与建角拦截)。区容量因此只有区服目录一个来源,区服列表的负载档、
 * 登录排队、建角拦截看的是同一行。
 *
 * <ul>
 *   <li><b>谁写</b>:每个区只由本区的网关副本写(写量随 zone 数线性增长);同一区的几个副本写的是同一个值,不需要选主。</li>
 *   <li><b>什么时候写</b>:每 {@code zone-policy.publish-interval-ms}(缺省 5 秒)一拍,读 etcd 里的现值,与区服目录这一行
 *       不同才写。读不产生 etcd 版本,稳态零写入;值被人手改坏、etcd 回档或第一次部署时下一拍自愈。</li>
 *   <li><b>不挂租约</b>:策略是持久配置,网关进程退出不该让它消失;login 读不到时退回软上限、不拦建角(见 Go 包注释)。</li>
 *   <li><b>区服目录里没有本区</b>:不写也不删。这时 assign-gate 回 404,玩家到不了 login;键里留着的旧值无害。</li>
 *   <li><b>失败</b>:一个区读写 etcd 或查库失败只记这个区,下一个区照常;日志按 {@link #ERROR_LOG_EVERY_MS} 节流。</li>
 * </ul>
 * 只有调度线程调 {@link #publish},字段不需要同步。
 */
@Component
public class ZonePolicyPublisher {

    private static final Logger log = LoggerFactory.getLogger(ZonePolicyPublisher.class);

    /** 与 go/login/internal/logic/pkg/zonepolicy 的 KeyPrefix 逐字相同(ZonePolicyPublisherTest 从 Go 源码核对)。 */
    public static final String KEY_PREFIX = "/mmorpg/zone_policy/zone/";

    /** 失败日志的最小间隔:etcd 不可用时每拍都会失败,不节流就是每 5 秒一条。持续多久看计数。 */
    static final long ERROR_LOG_EVERY_MS = 60_000L;

    /** 一拍里一个区的结果,也是计数 gateway_zone_policy_publish_total 的 outcome 标签(封闭集合)。 */
    enum Outcome {
        WRITTEN("written"),
        UNCHANGED("unchanged"),
        ZONE_MISSING("zone_missing"),
        ERROR("error");

        final String label;

        Outcome(String label) {
            this.label = label;
        }
    }

    private final ZoneConfigRepository zoneConfigRepo;
    private final HomeZoneProperties homeZones;
    private final ZonePolicyStore store;
    private final LongSupplier monotonicMs;
    /** 不用 Spring 的全局 ObjectMapper:它按 SNAKE_CASE 改名,会把字段写成 create_role_closed,与 Go 的契约对不上。 */
    private final ObjectMapper json = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);
    private final Map<Outcome, Counter> outcomes = new EnumMap<>(Outcome.class);
    private long lastErrorLogMs = Long.MIN_VALUE;

    @Autowired
    public ZonePolicyPublisher(ZoneConfigRepository zoneConfigRepo, HomeZoneProperties homeZones,
                               ZonePolicyStore store, MeterRegistry registry) {
        this(zoneConfigRepo, homeZones, store, registry, () -> System.nanoTime() / 1_000_000L);
    }

    ZonePolicyPublisher(ZoneConfigRepository zoneConfigRepo, HomeZoneProperties homeZones,
                        ZonePolicyStore store, MeterRegistry registry, LongSupplier monotonicMs) {
        this.zoneConfigRepo = zoneConfigRepo;
        this.homeZones = homeZones;
        this.store = store;
        this.monotonicMs = monotonicMs;
        for (Outcome outcome : Outcome.values()) {
            outcomes.put(outcome, Counter.builder("gateway.zone_policy.publish")
                    .description("Zone policy publications to etcd per home zone and tick, by outcome")
                    .tag("outcome", outcome.label)
                    .register(registry));
        }
    }

    /** 本区的区服策略键。 */
    public static String key(long zoneId) {
        return KEY_PREFIX + zoneId;
    }

    @Scheduled(fixedDelayString = "${zone-policy.publish-interval-ms:5000}")
    public void publish() {
        for (Long zoneId : homeZones.getHomeZoneIds()) {
            Outcome outcome;
            try {
                outcome = publishZone(zoneId);
            } catch (RuntimeException e) {
                outcome = Outcome.ERROR;
                long now = monotonicMs.getAsLong();
                if (lastErrorLogMs == Long.MIN_VALUE || now - lastErrorLogMs >= ERROR_LOG_EVERY_MS) {
                    lastErrorLogMs = now;
                    log.error("Zone policy publication failed for zone {} (login keeps its last known policy); "
                            + "further failures are suppressed for {} ms, see gateway_zone_policy_publish_total{outcome=\"error\"}",
                            zoneId, ERROR_LOG_EVERY_MS, e);
                }
            }
            outcomes.get(outcome).increment();
        }
    }

    /** 发布一个区:与 etcd 现值相同就不写。 */
    Outcome publishZone(long zoneId) {
        Optional<ZoneConfig> row = zoneConfigRepo.findById(zoneId);
        if (row.isEmpty()) {
            return Outcome.ZONE_MISSING;
        }
        ZonePolicy desired = ZonePolicy.of(row.get());
        String key = key(zoneId);
        Optional<byte[]> current = store.get(key);
        if (current.isPresent() && desired.equals(parse(current.get()))) {
            return Outcome.UNCHANGED;
        }
        store.put(key, serialize(desired));
        log.info("Zone policy published: zone={} capacity={} createRoleClosed={} (previous value {})",
                zoneId, desired.capacity(), desired.createRoleClosed(), current.isPresent() ? "differed" : "absent");
        return Outcome.WRITTEN;
    }

    byte[] serialize(ZonePolicy policy) {
        try {
            return json.writeValueAsBytes(policy);
        } catch (IOException e) {
            throw new IllegalStateException("cannot serialize zone policy " + policy, e);
        }
    }

    /** 解析 etcd 现值;解析不了回 null,按「不同」处理、整值重写。 */
    private ZonePolicy parse(byte[] raw) {
        try {
            return json.readValue(raw, ZonePolicy.class);
        } catch (IOException e) {
            return null;
        }
    }
}
