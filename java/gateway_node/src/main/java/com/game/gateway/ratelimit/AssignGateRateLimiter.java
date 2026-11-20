package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.util.EnumMap;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.LongSupplier;
import java.util.function.LongUnaryOperator;

/**
 * {@code /api/login} 与 {@code /api/assign-gate} 的前置限流,依次判:
 *
 * <ol>
 *   <li>开服波次:区不在当前波次 → 排队(retry_after 由距开放的毫秒算出,见下「排队提示」;queue_pos = -1);</li>
 *   <li>IP 令牌桶(全部 gateway 共享)空 → 429 {@code IP_RATE_LIMIT};</li>
 *   <li>区令牌桶(全部 gateway 共享;zone_id = 0 跳过)空 → 排队(retry_after 由区桶补到一个令牌要等的毫秒算出),
 *       这时 IP 桶只扣 {@code queue-retry-ip-cost-milli}(缺省 0.1 个令牌);</li>
 *   <li>账号冷却(进程内):同一端点上同一(账号, 客户端 IP)距上次放行不足冷却窗口 → 429 {@code ACCOUNT_COOLDOWN}。</li>
 * </ol>
 *
 * <p><b>排队提示(2026-11-19)。</b>两种排队的 retry_after_ms 都是 max(下限, 原始等待) 再加 [0, 抖动上界) 的均匀随机量
 * (缺省下限 2000ms、抖动 2000ms,{@code queue-retry-min-ms} / {@code queue-retry-jitter-ms})。以前原样下发「区桶补到一个令牌
 * 要等的毫秒」,2000 rps 时只有 1ms,客户端照做就是每个往返重试一次,区桶空时 IP 令牌又全额退还、IP 桶拦不住,排队的人越多,
 * 打到同一个 {@code rl:zone:<id>} 键上的请求越多,超过一台 Redis 主节点后命令超时、整体 fail-open,正好在最该限流时放开。
 * 有了下限,守规矩的客户端排队时每 2~4 秒一次,这个键的负载约为「排队人数 / 3 秒」;随机量把同一时刻被拒的一批人
 * (波次开放那一刻、区桶刚被抽空时)摊开,不再整批同时回来。区桶空时 IP 桶扣一小份,不守提示的单个 IP 最多打
 * ip-rps × 1000 / 这一份 次/秒(缺省 50),之后被 IP 桶 429,429 只读 IP 桶、不碰区桶。
 *
 * <p><b>两个桶在一次 {@link RateLimitStore#admit} 里判</b>(Redis 上 2026-11-12 起每个桶一段单键 Lua,区桶空时退还 IP 令牌,
 * 两键可以在 Redis Cluster 的不同节点上,见 {@link RedisRateLimitStore})。
 * 2026-11-05 起先 IP 后区(服务器缺口清单第 29 条,同时了结开服限流设计的 R13 / J1):以前先扣区桶,被 IP 桶拒掉的请求
 * 也已经吃掉一个区令牌,单个 IP 狂发就能把全区的桶掏空、让所有人排队。区桶空时 IP 令牌只扣一小份(2026-11-19 前是全额退还):
 * 排队中的重试几乎不吃 IP 桶,同一 NAT 后面的一群人等区桶时不会被 429 踢出排队,不守提示的单个 IP 也刷不动区桶。
 * 判定顺序与 Java 版(xuanming-server-mmo 的 RateLimiter)一致;排队提示的下限、抖动与 IP 扣一小份 Java 版待做。
 *
 * <p><b>Redis 出错 fail-open。</b>限流是保护手段,不能因为它不可用把全服挡在门外:存储抛异常时放过两个桶,之后
 * {@link #FAIL_OPEN_HOLD_MS} 内不再碰 Redis、直接放过两个桶(熔断);到期只放一个请求去探(半开),探通即恢复限流。
 * 没有熔断时,Redis 卡死期间每个请求都要等满命令超时才放行,请求线程被逐个占住,持续高流量下会占满 Tomcat 线程池。
 * 进程内的账号冷却不依赖 Redis,fail-open 期间照判。
 *
 * <p><b>账号冷却刻意不做成分布式。</b>它是挡客户端重试风暴的第一道过滤,权威去重在 go-zero login 的 {@code RedisLocker};
 * 每个副本各一份,N 个副本等于 N 倍窗口,账号刷量本来就被下游的区桶封住。
 *
 * <p><b>冷却键为什么带客户端 IP(G4b-J2)。</b>{@code account} 在这一步未经认证。只按账号计时的话,任何人每隔一个冷却
 * 窗口替别人的账号调一次接口,对方在同一副本上就一直拿 {@code ACCOUNT_COOLDOWN} —— 限流一打开就多出一个定点封号面。
 * 键里带上 IP 之后,这样刷只能挡住同一个 IP 上的这个账号,也就是刷的人自己。代价:同一账号换网络后不共享冷却窗口;
 * 同一出口 IP(NAT)后面的人仍能互相挡;所有请求都解析成同一个 IP 时(部署在 LB 之后却没配可信代理,见
 * {@link ClientIpResolver})退化为只按账号。
 *
 * <p><b>计数。</b>{@code gateway_rate_limit_decisions_total{outcome}}:每次判定记一个结果;
 * {@code gateway_rate_limit_fail_open_total{reason}}:放过两个桶的请求,store_error = 这次碰 Redis 出错,
 * bypass = 熔断期间没碰 Redis。标签只有固定的几个值。限流关闭时不计数。
 *
 * <p>线程安全。
 */
public class AssignGateRateLimiter {

    /** Redis 出错后直接放过两个桶多久(毫秒);与 Java 版 RateLimiter 的 FAIL_OPEN_HOLD 相同。 */
    static final long FAIL_OPEN_HOLD_MS = 5_000;

    /** {@link #bypassUntilMs} 的「没在熔断」取值。单调时钟的读数可以是任何 long,所以不拿 0 当哨兵。 */
    private static final long NOT_BYPASSING = Long.MIN_VALUE;

    private static final Logger log = LoggerFactory.getLogger(AssignGateRateLimiter.class);

    /** 一次判定的结果,即 {@code gateway_rate_limit_decisions_total} 的 outcome 标签。 */
    enum Outcome {
        PASS("pass"),
        WAVE_QUEUE("wave_queue"),
        ZONE_QUEUE("zone_queue"),
        IP_DENY("ip_deny"),
        COOLDOWN_DENY("cooldown_deny");

        final String label;

        Outcome(String label) {
            this.label = label;
        }
    }

    private final RateLimitStore store;   // null = 限流关闭
    private final RateLimitProperties props;
    private final WaveSchedule wave;
    private final LongSupplier monotonicMs;
    /** 给一个正的上界 n,返回 [0, n) 里的均匀随机数;只用来算排队提示的随机量。 */
    private final LongUnaryOperator randomBelow;
    private final Map<Outcome, Counter> decisions = new EnumMap<>(Outcome.class);
    private final Counter storeErrors;
    private final Counter bypassed;
    /** NOT_BYPASSING = 正常限流;否则熔断到这个单调时刻(毫秒),到期由抢到 CAS 的那一个请求去探。 */
    private final AtomicLong bypassUntilMs = new AtomicLong(NOT_BYPASSING);
    /** 冷却键 → 上次放行的单调时刻(毫秒)。 */
    private final ConcurrentMap<String, Long> accountCooldown = new ConcurrentHashMap<>();

    /**
     * @param store       全部 gateway 共享的令牌桶;null = 限流关闭,一律放行
     * @param monotonicMs 单调时钟(毫秒),熔断与账号冷却都用它;生产传 {@code System.nanoTime() / 1_000_000}
     * @param randomBelow 给正的上界 n 返回 [0, n) 的均匀随机数,算排队提示的随机量;生产传
     *                    {@code n -> ThreadLocalRandom.current().nextLong(n)},须线程安全
     */
    public AssignGateRateLimiter(RateLimitStore store, RateLimitProperties props, WaveSchedule wave,
                                 MeterRegistry registry, LongSupplier monotonicMs, LongUnaryOperator randomBelow) {
        this.store = store;
        this.props = props;
        this.wave = wave;
        this.monotonicMs = monotonicMs;
        this.randomBelow = randomBelow;
        for (Outcome outcome : Outcome.values()) {
            decisions.put(outcome, Counter.builder("gateway.rate_limit.decisions")
                    .description("前置限流每次判定的结果")
                    .tag("outcome", outcome.label)
                    .register(registry));
        }
        storeErrors = failOpenCounter(registry, "store_error");
        bypassed = failOpenCounter(registry, "bypass");
    }

    /** 冷却 scope 缺省为 assign,见 {@link #check(long, String, String, String)}。 */
    public RateLimitDecision check(long zoneId, String clientIp, String account) {
        return check(zoneId, clientIp, account, "assign");
    }

    /**
     * @param zoneId        目标区;0 = 任选一区,不判区桶(选定的区在下游照样受容量约束)
     * @param clientIp      客户端 IP({@link ClientIpResolver} 的结果);null / 空白按占位值 "?" 处理
     * @param account       未经认证的账号标识;null / 空白 = 不判冷却
     * @param cooldownScope 账号冷却的命名空间。/api/login 与 /api/assign-gate 共用本单例,若共用同一冷却键,正常的
     *                      「login 成功 → 紧接着 assign-gate」会在第二步必然命中 ACCOUNT_COOLDOWN(缺省 5s)被 429;
     *                      按端点分开后,各端点保留自己的防重试风暴窗口
     */
    public RateLimitDecision check(long zoneId, String clientIp, String account, String cooldownScope) {
        if (!props.isEnabled() || store == null) {
            return RateLimitDecision.pass();
        }
        if (!wave.isOpen(zoneId)) {
            return count(Outcome.WAVE_QUEUE, RateLimitDecision.queue(queueRetryAfterMs(wave.secondsUntilOpen(zoneId) * 1000L), -1));
        }

        // IP 桶键与冷却键共用同一个归一化后的 IP。ClientIpResolver 约定永不返回 null;万一传进来 null / 空白,统一落到
        // "?" 这一个占位值:这些请求共用一个 IP 桶,冷却退化为只按账号 —— 既不抛异常,也不会因为缺 IP 而绕过冷却或各得一个新键。
        String ip = (clientIp == null || clientIp.isBlank()) ? "?" : clientIp;
        long now = monotonicMs.getAsLong();

        Admission admission = admit(zoneId, ip, now);
        if (admission.kind() == Admission.Kind.IP_EMPTY) {
            return count(Outcome.IP_DENY, RateLimitDecision.deny("IP_RATE_LIMIT"));
        }
        if (admission.kind() == Admission.Kind.ZONE_EMPTY) {
            // queue_pos 只是提示值(区桶空时恒为 0),不是真实排位,见开服限流设计 §三。
            return count(Outcome.ZONE_QUEUE, RateLimitDecision.queue(queueRetryAfterMs(admission.waitMs()), 0));
        }
        if (inCooldown(cooldownScope, account, ip, now)) {
            return count(Outcome.COOLDOWN_DENY, RateLimitDecision.deny("ACCOUNT_COOLDOWN"));
        }
        return count(Outcome.PASS, RateLimitDecision.pass());
    }

    /**
     * 排队提示的 retry_after_ms:不短于下限,再加 [0, 抖动上界) 的均匀随机量(见类注释「排队提示」)。
     *
     * @param waitMs 原始等待:距波次开放的毫秒,或区桶补到一个令牌的毫秒
     */
    private long queueRetryAfterMs(long waitMs) {
        long jitterBound = props.getQueueRetryJitterMs();
        long jitter = jitterBound > 0 ? randomBelow.applyAsLong(jitterBound) : 0;
        return Math.max(props.getQueueRetryMinMs(), waitMs) + jitter;
    }

    /** 判两个桶;Redis 出错或熔断期间按放行处理(fail-open,见类注释)。 */
    private Admission admit(long zoneId, String ip, long now) {
        long until = bypassUntilMs.get();
        long probeMark = NOT_BYPASSING;
        if (until != NOT_BYPASSING) {
            // 熔断中:到期前一律放过;到期后只有抢到 CAS 的一个请求去探,其余照旧放过,直到它探完。
            probeMark = now + FAIL_OPEN_HOLD_MS;
            if (now < until || !bypassUntilMs.compareAndSet(until, probeMark)) {
                bypassed.increment();
                return Admission.ok();
            }
        }
        Admission admission;
        try {
            admission = store.admit(ipBucket(ip), zoneId > 0 ? zoneBucket(zoneId) : null);
        } catch (RuntimeException e) {
            bypassUntilMs.set(now + FAIL_OPEN_HOLD_MS);
            storeErrors.increment();
            log.warn("rate-limit fail-open: store error, bypassing token buckets without touching Redis for {} ms: {}",
                    FAIL_OPEN_HOLD_MS, e.toString());
            return Admission.ok();
        }
        // 只清自己写下的探测标记:探测期间若别的请求又出错、重新熔断,标记已被改写,CAS 不成功,熔断照旧。
        if (probeMark != NOT_BYPASSING && bypassUntilMs.compareAndSet(probeMark, NOT_BYPASSING)) {
            log.info("rate-limit store recovered, token buckets enforced again");
        }
        return admission;
    }

    /**
     * 进程内冷却:同一端点、同一(账号, IP)距上次放行不足冷却窗口 → true;否则记下这次放行的时刻,返回 false。
     * get / put 不是原子的,同一键上并发的两个请求可能都放行:这是尽力过滤,不提供互斥。
     */
    private boolean inCooldown(String scope, String account, String ip, long now) {
        if (account == null || account.isBlank()) {
            return false;
        }
        // IP 放在末尾:地址字面量与占位值都不含 '@',所以账号里带 '@'(如邮箱)也不会和别的 (账号, IP) 拼成同一个键。
        // 唯一能含 '@' 的 IP 是 ClientIpResolver 在可信代理之后原样返回的非字面量 XFF 段;能把这种值送进来的一方本来就能
        // 填任意 IP,键撞不撞都不多给它什么。
        String key = scope + ":" + account + "@" + ip;
        Long prev = accountCooldown.get(key);
        if (prev != null && now - prev < props.getAccountCooldownMs()) {
            return true;
        }
        accountCooldown.put(key, now);
        // 顺手回收:条目数每到 1024 的整数倍,删掉已过冷却窗口的条目。
        if ((accountCooldown.size() & 0x3FF) == 0) {
            long stale = now - props.getAccountCooldownMs();
            accountCooldown.entrySet().removeIf(e -> e.getValue() < stale);
        }
        return false;
    }

    private Bucket zoneBucket(long zoneId) {
        long rps = props.getZoneDefaultRps();
        long burst = props.getZoneDefaultBurst();
        RateLimitProperties.ZoneLimit override = props.getZoneOverrides().get(zoneId);
        if (override != null) {
            if (override.getRps() > 0) rps = override.getRps();
            if (override.getBurst() > 0) burst = override.getBurst();
        }
        return new Bucket("rl:zone:" + zoneId, rps, burst);
    }

    private Bucket ipBucket(String ip) {
        return new Bucket("rl:ip:" + ip, props.getIpRps(), props.getIpBurst());
    }

    private RateLimitDecision count(Outcome outcome, RateLimitDecision decision) {
        decisions.get(outcome).increment();
        return decision;
    }

    private static Counter failOpenCounter(MeterRegistry registry, String reason) {
        return Counter.builder("gateway.rate_limit.fail_open")
                .description("放过两个令牌桶的请求数(Redis 出错或熔断期间)")
                .tag("reason", reason)
                .register(registry);
    }
}
