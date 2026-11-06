package com.game.gateway.ratelimit;

import io.lettuce.core.ClientOptions;
import io.lettuce.core.RedisClient;
import io.lettuce.core.RedisURI;
import io.lettuce.core.TimeoutOptions;
import io.lettuce.core.api.StatefulRedisConnection;
import io.lettuce.core.codec.ByteArrayCodec;
import io.micrometer.core.instrument.MeterRegistry;
import java.time.Duration;
import java.util.Map;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * 前置限流的装配:专用 Lettuce 连接 → {@link RedisRateLimitStore}(令牌桶 Lua)→ {@link AssignGateRateLimiter}。
 *
 * <p>只在 {@code gate.rate-limit.enabled=true} 时建 Redis 连接与存储;关闭时限流器拿到 null 存储,一律放行
 * (见 {@link AssignGateRateLimiter#check})。
 *
 * <p>为什么另建一个 Lettuce 客户端,而不复用 {@code spring-boot-starter-data-redis} 的连接:限流在请求线程上同步调用,
 * 需要单独约束命令超时、断线行为与在途命令上限(见下面三个常量),不能跟着应用其它 Redis 用途的缺省走。
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

    /**
     * 限流器每条 Redis 命令的等待上限(2026-10-15 定,2026-11-05 改为约束令牌桶 Lua)。
     *
     * <p>限流在 Tomcat 请求线程上同步调用(LoginController、AssignGateController)。Lettuce 6.4.2 缺省不开命令超时
     * (TimeoutOptions.DEFAULT_TIMEOUT_COMMANDS=false),断线时还会把命令缓存起来等重连(DisconnectedBehavior.DEFAULT)。
     * 两者叠加:Redis 卡住、断线或连接半开时,每个 login / assign-gate 请求都挂在这里,请求线程被逐个占住直到 Tomcat 线程池
     * 耗尽,连不碰 Redis 的端点和 liveness 一起超时,副本被一起杀掉 —— 与 fail-open 的承诺(Redis 异常 → 放行)正好相反。
     *
     * <p>取 300ms(经验值,待压测确认):同集群 Redis 正常往返是亚毫秒级,300ms 只在 Redis 已经不正常时触发。一次检查只有
     * 一条 EVALSHA(两个桶在同一段 Lua 里判),超时以异常形式落进 {@link AssignGateRateLimiter} 的 fail-open,并触发熔断:
     * 之后 {@link AssignGateRateLimiter#FAIL_OPEN_HOLD_MS} 内不再碰 Redis,到期只放一个请求去探。所以 Redis 完全不回包时,
     * 只有熔断前已在途的请求和每轮的那一个探测请求各多等约 300ms,其余请求不等;连接半开(没有 RST)时 Lettuce 不会因命令超时而
     * 重连,要等内核判定连接死亡后才自动重连,在那之前每轮探测都超时、限流持续 fail-open。
     *
     * <p>这个值同时写进 RedisURI,Lettuce 用它约束建连后的握手(HELLO / AUTH):启动与自动重连时握手超过 300ms 即算失败 ——
     * 启动期失败即拒启(spec_G4b §9 R5 的启动期依赖,懒连接留作后续),重连失败按退避再试。
     *
     * <p>EVALSHA 遇到 NOSCRIPT(Redis 重启、脚本缓存被清)时再带全文 EVAL 一次,这一次检查是两个往返,各受本超时约束。
     */
    static final Duration REDIS_COMMAND_TIMEOUT = Duration.ofMillis(300);

    /**
     * 限流专用连接在协议栈里最多挂多少条未完成的命令(2026-10-15)。正常时单副本在途命令不超过 Tomcat 线程数(缺省 200),
     * 1024 不会误触;连接半开时超时的命令不会出栈,没有上限就会随请求量无界堆积(有未确认数据时 Linux 缺省重传约 15 分钟
     * 才判死连接)。超限的命令由 Lettuce 立即以 RedisException 失败,落进 {@link AssignGateRateLimiter} 的 fail-open。
     */
    static final int REDIS_REQUEST_QUEUE_SIZE = 1024;

    @Bean(destroyMethod = "shutdown")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public RedisClient rateLimitRedisClient() {
        log.info("rate-limit Redis client -> {}:{} (command timeout {} ms, commands rejected while disconnected)",
                redisHost, redisPort, REDIS_COMMAND_TIMEOUT.toMillis());
        RedisClient client = RedisClient.create(rateLimitRedisUri(redisHost, redisPort, redisPassword));
        client.setOptions(rateLimitClientOptions());
        return client;
    }

    /**
     * 限流专用连接的地址、口令与缺省超时(RedisURI 缺省 60s)。真正约束异步命令的是 {@link #rateLimitClientOptions()}
     * 的 TimeoutOptions;这里设成同一个值,免得连接的缺省超时仍是 60s、两处各说各话。包内可见,供单测钉住。
     */
    static RedisURI rateLimitRedisUri(String host, int port, String password) {
        RedisURI.Builder builder = RedisURI.builder()
                .withHost(host)
                .withPort(port)
                .withTimeout(REDIS_COMMAND_TIMEOUT);
        if (password != null && !password.isBlank()) {
            builder.withPassword(password.toCharArray());
        }
        return builder.build();
    }

    /**
     * 限流专用连接的客户端选项。包内可见,供单测钉住。
     * <ul>
     *   <li>REJECT_COMMANDS:断线期间命令立即失败,不进缓冲等重连 —— 立即抛异常、走 fail-open;
     *       自动重连保持缺省开启,Redis 恢复后限流自动恢复;</li>
     *   <li>TimeoutOptions:对所有命令启用 {@link #REDIS_COMMAND_TIMEOUT} 固定超时,Redis 卡住时命令以异常结束,
     *       不再无限期占住请求线程;</li>
     *   <li>requestQueueSize:给协议栈里未完成的命令设上限 {@link #REDIS_REQUEST_QUEUE_SIZE},连接半开时超时命令不再无界堆积。</li>
     * </ul>
     */
    static ClientOptions rateLimitClientOptions() {
        return ClientOptions.builder()
                .disconnectedBehavior(ClientOptions.DisconnectedBehavior.REJECT_COMMANDS)
                .timeoutOptions(TimeoutOptions.enabled(REDIS_COMMAND_TIMEOUT))
                .requestQueueSize(REDIS_REQUEST_QUEUE_SIZE)
                .build();
    }

    @Bean(destroyMethod = "close")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public StatefulRedisConnection<byte[], byte[]> rateLimitConnection(RedisClient client) {
        return client.connect(ByteArrayCodec.INSTANCE);
    }

    @Bean
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public RateLimitStore rateLimitStore(StatefulRedisConnection<byte[], byte[]> conn) {
        return new RedisRateLimitStore(conn.sync());
    }

    @Bean
    public AssignGateRateLimiter assignGateRateLimiter(ObjectProvider<RateLimitStore> store,
                                                       RateLimitProperties props,
                                                       MeterRegistry registry) {
        RateLimitStore s = store.getIfAvailable();
        if (s == null) {
            log.info("RateLimiter disabled (gate.rate-limit.enabled=false) — pass-through mode");
        } else {
            requireValidLimits(props);
        }
        long boot = System.currentTimeMillis() / 1000L;
        WaveSchedule wave = new WaveSchedule(props.getWave(), boot);
        return new AssignGateRateLimiter(s, props, wave, registry, () -> System.nanoTime() / 1_000_000L);
    }

    /**
     * 限流打开时,桶阈值必须为正,否则拒启(2026-11-05)。rps 为 0 时 Lua 里补充与等待都要除以它;burst 为 0 的桶永远不足
     * 一个令牌,等于把这一层全部挡死。单区覆盖的 rps / burst 写 0 表示沿用缺省,负数视为写错。包内可见,供单测钉住。
     */
    static void requireValidLimits(RateLimitProperties props) {
        requirePositive("zone-default-rps", props.getZoneDefaultRps());
        requirePositive("zone-default-burst", props.getZoneDefaultBurst());
        requirePositive("ip-rps", props.getIpRps());
        requirePositive("ip-burst", props.getIpBurst());
        for (Map.Entry<Long, RateLimitProperties.ZoneLimit> e : props.getZoneOverrides().entrySet()) {
            RateLimitProperties.ZoneLimit limit = e.getValue();
            if (limit == null || limit.getRps() < 0 || limit.getBurst() < 0) {
                throw new IllegalStateException("gate.rate-limit.zone-overrides." + e.getKey()
                        + " 的 rps / burst 不能为负(0 = 沿用缺省)");
            }
        }
    }

    private static void requirePositive(String key, long value) {
        if (value <= 0) {
            throw new IllegalStateException("gate.rate-limit." + key + " 必须为正,当前 " + value);
        }
    }
}
