package com.game.gateway.ratelimit;

import io.github.bucket4j.distributed.ExpirationAfterWriteStrategy;
import io.github.bucket4j.distributed.proxy.ProxyManager;
import io.github.bucket4j.redis.lettuce.cas.LettuceBasedProxyManager;
import io.lettuce.core.RedisClient;
import io.lettuce.core.RedisURI;
import io.lettuce.core.api.StatefulRedisConnection;
import io.lettuce.core.codec.ByteArrayCodec;
import java.time.Duration;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * Wires up Bucket4j over the same Redis the rest of the gateway uses.
 *
 * <p>Only activates when {@code gate.rate-limit.enabled=true}. When disabled
 * the limiter is constructed with a null ProxyManager and falls back to a
 * pass-through (see {@link AssignGateRateLimiter#check}).
 *
 * <p>Why a separate Lettuce client (instead of reusing
 * {@code spring-boot-starter-data-redis}'s connection): Spring's auto-config
 * gives a {@code RedisConnectionFactory}/{@code LettuceConnectionFactory},
 * but Bucket4j's Lettuce integration wants a raw
 * {@link StatefulRedisConnection}. Building a small dedicated Lettuce client
 * is the documented path and keeps Bucket4j's wire format isolated from any
 * Spring serialization the rest of the app may layer on later.
 */
@Configuration
@EnableConfigurationProperties(RateLimitProperties.class)
public class RateLimitConfig {

    private static final Logger log = LoggerFactory.getLogger(RateLimitConfig.class);

    @Value("${spring.data.redis.host:localhost}")
    private String redisHost;

    @Value("${spring.data.redis.port:6379}")
    private int redisPort;

    @Value("${spring.data.redis.password:}")
    private String redisPassword;

    @Bean(destroyMethod = "shutdown")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public RedisClient bucket4jRedisClient() {
        RedisURI.Builder builder = RedisURI.builder().withHost(redisHost).withPort(redisPort);
        if (redisPassword != null && !redisPassword.isBlank()) {
            builder.withPassword(redisPassword.toCharArray());
        }
        log.info("Bucket4j Redis client -> {}:{}", redisHost, redisPort);
        return RedisClient.create(builder.build());
    }

    @Bean(destroyMethod = "close")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public StatefulRedisConnection<byte[], byte[]> bucket4jConnection(RedisClient client) {
        return client.connect(ByteArrayCodec.INSTANCE);
    }

    /**
     * 桶空闲多久后允许 Redis 回收。取 1 小时:远大于任何一个桶的重填周期
     * (ip-rps/zone-rps 都是秒级),所以正在用的桶永远不会被提前删掉;
     * 又足够短,使一次性 IP / 一次性 zone 的桶不会长期占着内存。
     */
    private static final Duration BUCKET_IDLE_TTL = Duration.ofHours(1);

    @Bean
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public ProxyManager<byte[]> bucket4jProxyManager(StatefulRedisConnection<byte[], byte[]> conn) {
        // 必须显式给过期策略,否则 rl:ip:* / rl:zone:* 是**永久 key**。
        //
        // 裸 build() 时 Bucket4j 的 AbstractRedisProxyManagerBuilder
        // 取不到 expirationStrategy,落到 ExpirationAfterWriteStrategy.none(),
        // 其 calculateTimeToLiveMillis 恒返回 -1;LettuceBasedProxyManager 在
        // ttl<=0 分支走的是不带 px 的 `SET key val NX`,于是每出现一个新的桶 key
        // 就在 Redis 里永久留一条。桶 key 里含客户端 IP(见 AssignGateRateLimiter
        // 的 ipKey),而 IP 本身可被 X-Forwarded-For 影响 —— 无上界增长且无自愈,
        // 只能人工 SCAN/DEL;而这个 Redis 与 login 的 token 是同一实例,
        // 撑到 maxmemory 会连带把登录态淘汰掉。
        //
        // basedOnTimeForRefillingBucketUpToMax:TTL = 把桶重填到满所需的时间 + 给定余量,
        // 保证"还可能被用到的桶"绝不会被提前回收。
        return LettuceBasedProxyManager.builderFor(conn)
                .withExpirationStrategy(
                        ExpirationAfterWriteStrategy.basedOnTimeForRefillingBucketUpToMax(BUCKET_IDLE_TTL))
                .build();
    }

    @Bean
    public AssignGateRateLimiter assignGateRateLimiter(
            org.springframework.beans.factory.ObjectProvider<ProxyManager<byte[]>> proxyManager,
            RateLimitProperties props) {
        long boot = System.currentTimeMillis() / 1000L;
        WaveSchedule wave = new WaveSchedule(props.getWave(), boot);
        ProxyManager<byte[]> pm = proxyManager.getIfAvailable();
        if (pm == null) {
            log.info("RateLimiter disabled (gate.rate-limit.enabled=false) — pass-through mode");
        }
        return new AssignGateRateLimiter(pm, props, wave);
    }
}
