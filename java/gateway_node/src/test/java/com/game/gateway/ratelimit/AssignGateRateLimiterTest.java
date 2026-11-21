package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.micrometer.core.instrument.simple.SimpleMeterRegistry;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.LongUnaryOperator;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * {@link AssignGateRateLimiter} 的行为用例:用回放预设结果的假存储代替 Redis,手动时钟代替单调时钟,不依赖真实时间。
 * 令牌桶本身的算术(补充、过期、等待时长)由 {@link RedisRateLimitStoreIntegrationTest} 对真实 Redis 验证。
 *
 * <p>钉住的可观察结果:限流关闭 → 放行;波次未开 → 排队;IP 桶空 → 429 IP_RATE_LIMIT;区桶空 → 排队;
 * 两种排队的 retry_after 不短于下限、再加 [0, 抖动上界) 的随机量;
 * 账号冷却按(端点, 账号, IP)计时;存储出错 → 放过两个桶并熔断,到期只放一个请求去探;fail-open 期间进程内冷却照判;
 * 交给存储的桶键与阈值(含单区覆盖、zone_id = 0 不判区桶)。
 */
class AssignGateRateLimiterTest {

    private RateLimitProperties props;
    private ScriptedStore store;
    private SimpleMeterRegistry registry;
    /** 单调时钟(毫秒),各用例手动推进。 */
    private AtomicLong clock;
    /** 排队提示的随机源;缺省恒取 0,要看随机量的用例自己换。 */
    private LongUnaryOperator random;

    @BeforeEach
    void setUp() {
        props = new RateLimitProperties();
        props.setEnabled(true);
        props.setZoneDefaultRps(100);
        props.setZoneDefaultBurst(200);
        props.setIpRps(2);
        props.setIpBurst(10);
        props.setAccountCooldownMs(5000);
        props.setQueueRetryMinMs(2000);
        props.setQueueRetryJitterMs(2000);
        store = new ScriptedStore();
        registry = new SimpleMeterRegistry();
        clock = new AtomicLong(1_000_000L);
        random = bound -> 0;
    }

    private AssignGateRateLimiter limiter() {
        return limiter(disabledWave());
    }

    private AssignGateRateLimiter limiter(WaveSchedule wave) {
        return new AssignGateRateLimiter(store, props, wave, registry, clock::get, bound -> random.applyAsLong(bound));
    }

    private static WaveSchedule disabledWave() {
        var cfg = new RateLimitProperties.WaveSchedule();
        cfg.setEnabled(false);
        return new WaveSchedule(cfg, Instant.now().getEpochSecond());
    }

    private double decisions(String outcome) {
        return registry.get("gateway.rate_limit.decisions").tag("outcome", outcome).counter().count();
    }

    private double failOpen(String reason) {
        return registry.get("gateway.rate_limit.fail_open").tag("reason", reason).counter().count();
    }

    @Test
    void disabledLimiter_alwaysPasses() {
        props.setEnabled(false);

        assertTrue(limiter().check(1, "10.0.0.1", "alice").isPass());
        assertEquals(0, store.calls.size(), "限流关闭时不碰存储");
        assertEquals(0, decisions("pass"), "限流关闭时不计数");
    }

    @Test
    void nullStore_alwaysPasses() {
        AssignGateRateLimiter limiter = new AssignGateRateLimiter(null, props, disabledWave(), registry, clock::get, random);
        assertTrue(limiter.check(1, "10.0.0.1", "alice").isPass());
    }

    @Test
    void waveClosed_queuesWithRetryAfter() {
        // 波次 30s 后才开始,第 0 档只放 zone 1。
        var cfg = new RateLimitProperties.WaveSchedule();
        cfg.setEnabled(true);
        cfg.setStartEpochSec(Instant.now().getEpochSecond() + 30);
        var step = new RateLimitProperties.WaveStep();
        step.setOffsetSec(0);
        step.setAllowZones(List.of(1L));
        cfg.setSchedule(List.of(step));

        var d = limiter(new WaveSchedule(cfg, cfg.getStartEpochSec())).check(2, "10.0.0.2", "bob");

        assertTrue(d.isQueue());
        assertEquals(3_600_000L, d.getRetryAfterMs(), "zone 2 不在任何一档里:一小时后再来(随机量取 0)");
        assertEquals(-1, d.getQueuePosEstimate());
        assertEquals(0, store.calls.size(), "波次未开不碰令牌桶");
        assertEquals(1, decisions("wave_queue"));
    }

    @Test
    void waveClosed_retryAfterIsTimeUntilOpenPlusJitter() {
        // 波次 30s 后开始,第 0 档放 zone 1。读秒时可能跨过一个秒边界,距开放是 29 或 30 秒。
        var cfg = new RateLimitProperties.WaveSchedule();
        cfg.setEnabled(true);
        cfg.setStartEpochSec(Instant.now().getEpochSecond() + 30);
        var step = new RateLimitProperties.WaveStep();
        step.setOffsetSec(0);
        step.setAllowZones(List.of(1L));
        cfg.setSchedule(List.of(step));
        random = bound -> bound - 1;

        var d = limiter(new WaveSchedule(cfg, cfg.getStartEpochSec())).check(1, "10.0.0.2", "bob");

        assertTrue(d.isQueue());
        long untilOpenMs = d.getRetryAfterMs() - 1999;
        assertTrue(untilOpenMs == 29_000 || untilOpenMs == 30_000,
                "距开放的毫秒再加随机量:开放那一刻回来的人被摊开,实际 " + d.getRetryAfterMs());
    }

    @Test
    void bucketsHandedToStore_ipFirstWithConfiguredLimits() {
        var override = new RateLimitProperties.ZoneLimit();
        override.setRps(2000);
        override.setBurst(5000);
        props.setZoneOverrides(Map.of(1L, override));
        AssignGateRateLimiter limiter = limiter();

        assertTrue(limiter.check(1, "10.0.0.8", "henry").isPass());
        assertTrue(limiter.check(3, "10.0.0.8", "henry", "login").isPass());

        Bucket[] overridden = store.calls.get(0);
        assertEquals(new Bucket("rl:ip:10.0.0.8", 2, 10), overridden[0], "IP 桶用 ip-rps / ip-burst");
        assertEquals(new Bucket("rl:zone:1", 2000, 5000), overridden[1], "有单区覆盖的区用覆盖值");
        assertEquals(new Bucket("rl:zone:3", 100, 200), store.calls.get(1)[1], "没有覆盖的区用缺省值");
        assertEquals(2, decisions("pass"));
    }

    @Test
    void zeroZone_onlyIpBucket() {
        assertTrue(limiter().check(0, "10.0.0.7", "grace").isPass());

        assertEquals(1, store.calls.size());
        assertEquals("rl:ip:10.0.0.7", store.calls.get(0)[0].key());
        assertNull(store.calls.get(0)[1], "zone_id = 0(任选一区)不判区桶");
    }

    @Test
    void blankIp_sharesPlaceholderBucket() {
        limiter().check(1, " ", null);
        limiter().check(1, null, null);

        assertEquals("rl:ip:?", store.calls.get(0)[0].key());
        assertEquals("rl:ip:?", store.calls.get(1)[0].key());
    }

    @Test
    void ipBucketEmpty_deniedAndDoesNotStartCooldown() {
        store.then(Admission.ipEmpty());
        AssignGateRateLimiter limiter = limiter();

        var d = limiter.check(1, "10.0.0.4", "dave");
        assertTrue(d.isDeny());
        assertEquals("IP_RATE_LIMIT", d.getReason());
        assertEquals(1, decisions("ip_deny"));

        // 被桶拒掉的请求不算放行,不开始冷却:同一账号同一 IP 紧接着再来,桶放行就放行。
        assertTrue(limiter.check(1, "10.0.0.4", "dave").isPass());
    }

    @Test
    void zoneBucketEmpty_queuesWithStoreWait() {
        store.then(Admission.zoneEmpty(2500));

        var d = limiter().check(1, "10.0.0.3", "carol");

        assertTrue(d.isQueue());
        assertEquals(2500, d.getRetryAfterMs(), "区桶补到一个令牌要等的毫秒长于下限时照用(随机量取 0)");
        assertEquals(0, d.getQueuePosEstimate(), "queue_pos 只是提示值,区桶空时恒为 0");
        assertEquals(1, decisions("zone_queue"));
    }

    @Test
    void zoneBucketEmpty_shortWaitIsRaisedToTheFloor() {
        store.then(Admission.zoneEmpty(1)); // 2000 rps 的区桶补到一个令牌只要不到 1ms

        var d = limiter().check(1, "10.0.0.3", "carol");

        assertTrue(d.isQueue());
        assertEquals(2000, d.getRetryAfterMs(), "不短于 queue-retry-min-ms:不让排队的人紧循环重试同一个区桶键");
    }

    @Test
    void queueHint_addsJitterBelowTheConfiguredBound() {
        List<Long> bounds = new ArrayList<>();
        random = bound -> {
            bounds.add(bound);
            return bound - 1;
        };
        store.then(Admission.zoneEmpty(1));

        var d = limiter().check(1, "10.0.0.3", "carol");

        assertEquals(List.of(2000L), bounds, "随机量的上界就是 queue-retry-jitter-ms");
        assertEquals(2000 + 1999, d.getRetryAfterMs(), "下限加随机量,随机量取不到上界本身");
    }

    @Test
    void zeroJitter_drawsNoRandom() {
        props.setQueueRetryJitterMs(0);
        random = bound -> {
            throw new AssertionError("抖动为 0 时不该取随机数(随机源只收正的上界)");
        };
        store.then(Admission.zoneEmpty(2500));

        assertEquals(2500, limiter().check(1, "10.0.0.3", "carol").getRetryAfterMs());
    }

    @Test
    void accountCooldown_perEndpointAccountAndIp_untilWindowEnds() {
        AssignGateRateLimiter limiter = limiter();

        assertTrue(limiter.check(1, "10.0.1.1", "ivy").isPass());
        // 同账号、不同 IP:各记各的冷却(G4b-J2),不应被另一个 IP 上的冷却挡住。
        assertTrue(limiter.check(1, "10.0.1.2", "ivy").isPass());
        // 同账号、同 IP、另一个端点:冷却按端点分开,login → assign-gate 的正常顺序不互相挡。
        assertTrue(limiter.check(1, "10.0.1.1", "ivy", "login").isPass());

        clock.addAndGet(4_999);
        var sameIp = limiter.check(1, "10.0.1.1", "ivy");
        assertTrue(sameIp.isDeny());
        assertEquals("ACCOUNT_COOLDOWN", sameIp.getReason());
        assertEquals(1, decisions("cooldown_deny"));

        // 窗口从上次放行起算;被冷却挡住的那次不续期。
        clock.addAndGet(1);
        assertTrue(limiter.check(1, "10.0.1.1", "ivy").isPass(), "满冷却窗口后放行");
    }

    @Test
    void storeError_failsOpenThenBypassesUntilHoldEnds_thenOneProbeRestoresLimiting() {
        store.thenThrow(new RuntimeException("redis down")).then(Admission.ok()).then(Admission.ipEmpty());
        AssignGateRateLimiter limiter = limiter();

        assertTrue(limiter.check(1, "10.0.0.6", null).isPass(), "存储出错必须放行(fail-open)");
        assertEquals(1, failOpen("store_error"));

        // 熔断期间不碰 Redis,直接放过两个桶。
        clock.addAndGet(AssignGateRateLimiter.FAIL_OPEN_HOLD_MS - 1);
        assertTrue(limiter.check(1, "10.0.0.6", null).isPass());
        assertEquals(1, store.calls.size(), "熔断期间不应访问存储");
        assertEquals(1, failOpen("bypass"));

        // 到期:这一个请求去探,探通即恢复限流 —— 之后存储的拒绝照常生效。
        clock.addAndGet(1);
        assertTrue(limiter.check(1, "10.0.0.6", null).isPass());
        assertEquals(2, store.calls.size(), "熔断到期应放一个请求去探");
        var limited = limiter.check(1, "10.0.0.6", null);
        assertTrue(limited.isDeny(), "探通之后恢复限流");
        assertEquals(3, store.calls.size());
    }

    @Test
    void failedProbe_reopensBreakerForAnotherHold() {
        store.thenThrow(new RuntimeException("redis down")).thenThrow(new RuntimeException("still down"));
        AssignGateRateLimiter limiter = limiter();

        limiter.check(1, "10.0.0.9", null);
        clock.addAndGet(AssignGateRateLimiter.FAIL_OPEN_HOLD_MS);
        assertTrue(limiter.check(1, "10.0.0.9", null).isPass(), "探测失败同样放行");
        assertEquals(2, failOpen("store_error"));

        clock.addAndGet(AssignGateRateLimiter.FAIL_OPEN_HOLD_MS - 1);
        assertTrue(limiter.check(1, "10.0.0.9", null).isPass());
        assertEquals(2, store.calls.size(), "探测失败后重新熔断一整段,期间不再访问存储");
    }

    @Test
    void failOpen_stillAppliesProcessLocalCooldown() {
        store.thenThrow(new RuntimeException("redis down"));
        AssignGateRateLimiter limiter = limiter();

        assertTrue(limiter.check(1, "10.0.0.5", "eve").isPass());
        var second = limiter.check(1, "10.0.0.5", "eve");

        assertTrue(second.isDeny(), "冷却在进程内,不依赖 Redis,fail-open 期间照判");
        assertEquals("ACCOUNT_COOLDOWN", second.getReason());
    }

    @Test
    void checkIpOnly_skipsWaveZoneAndCooldown_andEnforcesSharedIpBucket() {
        // 波次尚未开放:checkIpOnly 不能被波次挡住(排队中续期也要能成功)。
        var cfg = new RateLimitProperties.WaveSchedule();
        cfg.setEnabled(true);
        cfg.setStartEpochSec(Instant.now().getEpochSecond() + 60);
        var step = new RateLimitProperties.WaveStep();
        step.setOffsetSec(0);
        step.setAllowZones(List.of(1L));
        cfg.setSchedule(List.of(step));
        AssignGateRateLimiter limiter = limiter(new WaveSchedule(cfg, cfg.getStartEpochSec()));

        store.then(Admission.ok()).then(Admission.ok()).then(Admission.ipEmpty());

        assertTrue(limiter.checkIpOnly("10.0.0.10").isPass());
        assertTrue(limiter.checkIpOnly("10.0.0.10").isPass(), "同一 IP 连续调用不受账号冷却阻拦");
        var denied = limiter.checkIpOnly("10.0.0.10");
        assertTrue(denied.isDeny());
        assertEquals("IP_RATE_LIMIT", denied.getReason());

        assertEquals(3, store.calls.size());
        for (Bucket[] call : store.calls) {
            assertEquals("rl:ip:10.0.0.10", call[0].key());
            assertNull(call[1], "checkIpOnly 不碰区桶");
        }
        assertEquals(2, decisions("pass"));
        assertEquals(1, decisions("ip_deny"));
    }

    @Test
    void checkIpOnly_disabledOrBlankIp() {
        props.setEnabled(false);
        assertTrue(limiter().checkIpOnly("10.0.0.10").isPass());
        assertEquals(0, store.calls.size());

        props.setEnabled(true);
        assertTrue(limiter().checkIpOnly(" ").isPass());
        assertEquals("rl:ip:?", store.calls.get(0)[0].key());
    }

    /** 按顺序回放预设结果的假存储(预设用完后一律放行),并记下每次调用的两个桶。 */
    private static final class ScriptedStore implements RateLimitStore {
        private final Deque<Object> results = new ArrayDeque<>();
        final List<Bucket[]> calls = new ArrayList<>();

        ScriptedStore then(Admission admission) {
            results.add(admission);
            return this;
        }

        ScriptedStore thenThrow(RuntimeException error) {
            results.add(error);
            return this;
        }

        @Override
        public Admission admit(Bucket ip, Bucket zone) {
            calls.add(new Bucket[] {ip, zone});
            Object next = results.isEmpty() ? Admission.ok() : results.poll();
            if (next instanceof RuntimeException error) {
                throw error;
            }
            return (Admission) next;
        }
    }
}
