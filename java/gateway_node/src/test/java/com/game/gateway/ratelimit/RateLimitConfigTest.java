package com.game.gateway.ratelimit;

import io.lettuce.core.ClientOptions;
import io.lettuce.core.RedisClient;
import io.lettuce.core.RedisURI;
import io.lettuce.core.TimeoutOptions;
import org.junit.jupiter.api.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.time.Duration;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 钉住限流专用 Lettuce 连接的超时与断线语义(2026-10-15),以及限流打开时的阈值校验(2026-11-05)。
 *
 * <p>为什么要钉:限流在 Tomcat 请求线程上同步调用。这里任何一项退回 Lettuce 缺省(连接缺省超时 60s、不开命令超时、
 * 断线时缓存命令等重连),Redis 卡住或断线时请求线程就会被逐个占住,直到线程池耗尽、liveness 超时、副本一起被杀,
 * 而不是按设计 fail-open 放行(原因详见 {@link RateLimitConfig#REDIS_COMMAND_TIMEOUT})。
 *
 * <p>不起 Spring 上下文、不连 Redis。
 */
class RateLimitConfigTest {

    private static final Duration EXPECTED_TIMEOUT = Duration.ofMillis(300);

    @Test
    void commandTimeoutIs300ms() {
        assertEquals(EXPECTED_TIMEOUT, RateLimitConfig.REDIS_COMMAND_TIMEOUT,
                "限流 Redis 命令超时应为 300ms;改值要同步评估 fail-open 前每个请求多等的时长");
    }

    @Test
    void redisUri_carriesCommandTimeoutInsteadOfLettuceDefault60s() {
        RedisURI uri = RateLimitConfig.rateLimitRedisUri("redis.infra", 6379, "");

        assertEquals("redis.infra", uri.getHost());
        assertEquals(6379, uri.getPort());
        assertEquals(EXPECTED_TIMEOUT, uri.getTimeout(),
                "RedisURI 不写 timeout 时是 Lettuce 缺省 60s");
    }

    @Test
    @SuppressWarnings("deprecation") // getPassword 在 Lettuce 6.x 标了弃用,这里只用来确认口令是否写上
    void redisUri_setsPasswordOnlyWhenNonBlank() {
        assertNull(RateLimitConfig.rateLimitRedisUri("h", 1, "").getPassword(), "空口令不应设置(dev 档 Redis 无口令)");
        assertNull(RateLimitConfig.rateLimitRedisUri("h", 1, "   ").getPassword(), "空白口令不应设置");
        assertNull(RateLimitConfig.rateLimitRedisUri("h", 1, null).getPassword(), "null 口令不应设置");
        assertArrayEquals("s3cret".toCharArray(), RateLimitConfig.rateLimitRedisUri("h", 1, "s3cret").getPassword(),
                "非空口令必须原样设置,否则 release 档 Redis 认证失败、限流一律 fail-open");
    }

    @Test
    void clientOptions_rejectCommandsWhileDisconnected() {
        ClientOptions options = RateLimitConfig.rateLimitClientOptions();

        assertEquals(ClientOptions.DisconnectedBehavior.REJECT_COMMANDS, options.getDisconnectedBehavior(),
                "断线时命令必须立即失败走 fail-open,不能进缓冲等重连");
        assertTrue(options.isAutoReconnect(), "自动重连必须保持开启,Redis 恢复后限流才能自动恢复");
    }

    @Test
    void clientOptions_enableFixedCommandTimeout() {
        TimeoutOptions timeouts = RateLimitConfig.rateLimitClientOptions().getTimeoutOptions();

        assertTrue(timeouts.isTimeoutCommands(), "必须对所有命令开超时:Lettuce 缺省不开");
        TimeoutOptions.TimeoutSource source = timeouts.getSource();
        // 固定超时源不看具体命令,传 null 即可取到配置值。
        long timeoutMillis = source.getTimeUnit().toMillis(source.getTimeout(null));
        assertEquals(EXPECTED_TIMEOUT.toMillis(), timeoutMillis, "命令超时应为固定 300ms");
    }

    @Test
    void clientOptions_boundPendingCommandQueue() {
        assertEquals(1024, RateLimitConfig.rateLimitClientOptions().getRequestQueueSize(),
                "协议栈里未完成命令的上限应为 1024:连接半开时超时命令不出栈,不设上限会在 384m 的堆里无界堆积");
    }

    @Test
    void redisClientBean_appliesTheseOptions() {
        // 上面几例只钉工厂函数的返回值;这一例钉住接线:删掉 rateLimitRedisClient 里的 setOptions,
        // 运行时就退回 Lettuce 缺省(断线缓存命令、不开命令超时),而前面几例照样全绿。
        // RedisClient.create 不建连,这里不需要 Redis。
        RateLimitConfig config = new RateLimitConfig();
        ReflectionTestUtils.setField(config, "redisHost", "localhost");
        ReflectionTestUtils.setField(config, "redisPort", 6379);
        ReflectionTestUtils.setField(config, "redisPassword", "");
        RedisClient client = config.rateLimitRedisClient();
        try {
            ClientOptions options = client.getOptions();
            assertEquals(ClientOptions.DisconnectedBehavior.REJECT_COMMANDS, options.getDisconnectedBehavior(),
                    "rateLimitRedisClient 必须把限流专用选项设到客户端上");
            assertTrue(options.getTimeoutOptions().isTimeoutCommands(), "客户端上必须开命令超时");
            assertEquals(1024, options.getRequestQueueSize(), "客户端上必须带未完成命令上限");
        } finally {
            client.shutdown();
        }
    }

    @Test
    void requireValidLimits_acceptsPackagedDefaults() {
        assertDoesNotThrow(() -> RateLimitConfig.requireValidLimits(new RateLimitProperties()),
                "Java 属性缺省(区 500 / 1000、IP 2 / 10)必须能通过校验");
    }

    @Test
    void requireValidLimits_rejectsNonPositiveBucketSettings() {
        // rps 为 0 时 Lua 里要除以它;burst 为 0 的桶永远不足一个令牌,等于挡死这一层。四个阈值逐个钉住。
        for (String key : new String[] {"zone-default-rps", "zone-default-burst", "ip-rps", "ip-burst"}) {
            RateLimitProperties props = new RateLimitProperties();
            switch (key) {
                case "zone-default-rps" -> props.setZoneDefaultRps(0);
                case "zone-default-burst" -> props.setZoneDefaultBurst(0);
                case "ip-rps" -> props.setIpRps(-1);
                default -> props.setIpBurst(0);
            }
            IllegalStateException e = assertThrows(IllegalStateException.class,
                    () -> RateLimitConfig.requireValidLimits(props), key + " 不为正时必须拒启");
            assertTrue(e.getMessage().contains(key), "报错要指出是哪个键:" + e.getMessage());
        }
    }

    @Test
    void requireValidLimits_zoneOverrideZeroMeansDefaultButNegativeIsRejected() {
        RateLimitProperties props = new RateLimitProperties();
        RateLimitProperties.ZoneLimit useDefaults = new RateLimitProperties.ZoneLimit();
        props.setZoneOverrides(Map.of(1L, useDefaults));
        assertDoesNotThrow(() -> RateLimitConfig.requireValidLimits(props), "覆盖写 0 表示沿用缺省,不算写错");

        RateLimitProperties.ZoneLimit negative = new RateLimitProperties.ZoneLimit();
        negative.setBurst(-5);
        props.setZoneOverrides(Map.of(7L, negative));
        IllegalStateException e = assertThrows(IllegalStateException.class,
                () -> RateLimitConfig.requireValidLimits(props), "覆盖里的负数必须拒启");
        assertTrue(e.getMessage().contains("zone-overrides.7"), "报错要指出是哪个区:" + e.getMessage());
    }
}
