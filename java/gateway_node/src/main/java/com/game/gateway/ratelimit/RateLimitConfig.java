package com.game.gateway.ratelimit;

import io.lettuce.core.AbstractRedisClient;
import io.lettuce.core.ClientOptions;
import io.lettuce.core.RedisClient;
import io.lettuce.core.RedisURI;
import io.lettuce.core.TimeoutOptions;
import io.lettuce.core.cluster.ClusterClientOptions;
import io.lettuce.core.cluster.ClusterTopologyRefreshOptions;
import io.lettuce.core.cluster.RedisClusterClient;
import io.micrometer.core.instrument.MeterRegistry;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.autoconfigure.data.redis.RedisProperties;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * 前置限流的装配:专用 Lettuce 客户端 → 一条连接({@link RateLimitRedis})→ {@link RedisRateLimitStore}(令牌桶 Lua)
 * → {@link AssignGateRateLimiter}。
 *
 * <p>只在 {@code gate.rate-limit.enabled=true} 时建 Redis 客户端、连接与存储;关闭时限流器拿到 null 存储,一律放行
 * (见 {@link AssignGateRateLimiter#check})。
 *
 * <p>为什么另建一个 Lettuce 客户端,而不复用 {@code spring-boot-starter-data-redis} 的连接:限流在请求线程上同步调用,
 * 需要单独约束命令超时、断线行为与在途命令上限(见下面三个常量),不能跟着应用其它 Redis 用途的缺省走。
 *
 * <p><b>连哪里(2026-11-12,docs/design/redis-cluster-all-domains-20261110.md §4.5、§5.3)。</b>地址只读 Spring 的
 * {@code spring.data.redis.*}({@link RedisProperties}),与自动配置的连接(健康检查用的那一条)同一个来源:
 * <ul>
 *   <li>写了 {@code spring.data.redis.cluster.nodes}:Redis Cluster,这些地址是种子,拓扑由客户端发现并刷新
 *       ({@link #rateLimitClusterClientOptions()});自动配置的连接同样按集群连;</li>
 *   <li>没写:单实例 {@code spring.data.redis.host:port}(本机开发,与还没上控制集群的部署)。</li>
 * </ul>
 * 口令两种形态都取 {@code spring.data.redis.password}。
 */
@Configuration
@EnableConfigurationProperties({RateLimitProperties.class, RedisProperties.class})
public class RateLimitConfig {

    private static final Logger log = LoggerFactory.getLogger(RateLimitConfig.class);

    /**
     * 限流器每条 Redis 命令的等待上限(2026-10-15 定,2026-11-05 改为约束令牌桶 Lua)。
     *
     * <p>限流在 Tomcat 请求线程上同步调用(LoginController、AssignGateController)。Lettuce 6.4.2 缺省不开命令超时
     * (TimeoutOptions.DEFAULT_TIMEOUT_COMMANDS=false),断线时还会把命令缓存起来等重连(DisconnectedBehavior.DEFAULT)。
     * 两者叠加:Redis 卡住、断线或连接半开时,每个 login / assign-gate 请求都挂在这里,请求线程被逐个占住直到 Tomcat 线程池
     * 耗尽,连不碰 Redis 的端点和 liveness 一起超时,副本被一起杀掉 —— 与 fail-open 的承诺(Redis 异常 → 放行)正好相反。
     *
     * <p>取 300ms(经验值,待压测确认):同集群 Redis 正常往返是亚毫秒级,300ms 只在 Redis 已经不正常时触发。一次检查最多
     * 三条 EVALSHA(扣 IP 桶、扣区桶、区桶空时退还 IP 令牌,2026-11-12 起每段脚本只碰一个键),每条各受本超时约束;
     * 超时以异常形式落进 {@link AssignGateRateLimiter} 的 fail-open,并触发熔断:之后 {@link AssignGateRateLimiter#FAIL_OPEN_HOLD_MS}
     * 内不再碰 Redis,到期只放一个请求去探。所以 Redis 完全不回包时,只有熔断前已在途的请求和每轮的那一个探测请求多等约 300ms
     * (第一条就超时;退还那一条超时不熔断,见 {@link RedisRateLimitStore}),其余请求不等;连接半开(没有 RST)时 Lettuce 不会因
     * 命令超时而重连,要等内核判定连接死亡后才自动重连,在那之前每轮探测都超时、限流持续 fail-open。
     *
     * <p>这个值同时写进 RedisURI,Lettuce 用它约束建连后的握手(HELLO / AUTH)与集群拓扑刷新的查询:启动与自动重连时握手超过
     * 300ms 即算失败 —— 启动期失败即拒启(spec_G4b §9 R5 的启动期依赖,懒连接留作后续),重连失败按退避再试。
     *
     * <p>EVALSHA 遇到 NOSCRIPT(Redis 重启、脚本缓存被清、集群里第一次落到某个节点)时再带全文 EVAL 一次,这一条是两个往返,
     * 各受本超时约束。
     */
    static final Duration REDIS_COMMAND_TIMEOUT = Duration.ofMillis(300);

    /**
     * 限流专用连接在协议栈里最多挂多少条未完成的命令(2026-10-15)。正常时单副本在途命令不超过 Tomcat 线程数(缺省 200),
     * 1024 不会误触;连接半开时超时的命令不会出栈,没有上限就会随请求量无界堆积(有未确认数据时 Linux 缺省重传约 15 分钟
     * 才判死连接)。超限的命令由 Lettuce 立即以 RedisException 失败,落进 {@link AssignGateRateLimiter} 的 fail-open。
     * 集群连接按节点各有一条,上限按条计。
     */
    static final int REDIS_REQUEST_QUEUE_SIZE = 1024;

    /**
     * 集群拓扑的定期刷新周期(2026-11-12)。迁槽、换主之后客户端先靠 MOVED / ASK 与重连触发的自适应刷新跟上
     * ({@link #rateLimitClusterClientOptions()}),定期刷新只兜底「一直没有请求落到变化的那几个槽」。Lettuce 缺省 60 秒。
     */
    static final Duration CLUSTER_TOPOLOGY_REFRESH_PERIOD = Duration.ofSeconds(60);

    /**
     * 限流专用的 Lettuce 客户端(只建客户端、不建连,建连在 {@link #rateLimitRedis})。写了集群种子是
     * {@link RedisClusterClient},否则是单实例 {@link RedisClient};两种都带上限流专用选项。种子写错拒启。
     */
    @Bean(destroyMethod = "shutdown")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public AbstractRedisClient rateLimitRedisClient(RedisProperties redis) {
        List<String> nodes = redis.getCluster() == null ? List.of() : redis.getCluster().getNodes();
        List<RedisURI> seeds = rateLimitClusterSeeds(nodes, redis.getPassword());
        if (seeds.isEmpty()) {
            log.info("rate-limit Redis client -> {}:{} (standalone, command timeout {} ms, commands rejected while disconnected)",
                    redis.getHost(), redis.getPort(), REDIS_COMMAND_TIMEOUT.toMillis());
            RedisClient client = RedisClient.create(rateLimitRedisUri(redis.getHost(), redis.getPort(), redis.getPassword()));
            client.setOptions(rateLimitClientOptions());
            return client;
        }
        log.info("rate-limit Redis cluster client -> {} seed(s) {} (command timeout {} ms, commands rejected while disconnected)",
                seeds.size(), nodes, REDIS_COMMAND_TIMEOUT.toMillis());
        RedisClusterClient client = RedisClusterClient.create(seeds);
        client.setOptions(rateLimitClusterClientOptions());
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
     * 把 {@code spring.data.redis.cluster.nodes} 换成种子 URI(超时与口令同 {@link #rateLimitRedisUri})。没写返回空表;
     * 有一项不是 {@code host:port} 就拒启,不跳过 —— 少一个种子不影响连通,但写错说明配置不是我们以为的那一份。包内可见,供单测钉住。
     */
    static List<RedisURI> rateLimitClusterSeeds(List<String> nodes, String password) {
        if (nodes == null || nodes.isEmpty()) {
            return List.of();
        }
        List<RedisURI> seeds = new ArrayList<>(nodes.size());
        for (String raw : nodes) {
            String node = raw == null ? "" : raw.trim();
            int colon = node.lastIndexOf(':');
            if (colon <= 0 || colon == node.length() - 1) {
                throw new IllegalStateException("spring.data.redis.cluster.nodes entry '" + node + "' is not host:port");
            }
            int port;
            try {
                port = Integer.parseInt(node.substring(colon + 1));
            } catch (NumberFormatException e) {
                throw new IllegalStateException("spring.data.redis.cluster.nodes entry '" + node + "' has a non-numeric port", e);
            }
            if (port < 1 || port > 65535) {
                throw new IllegalStateException("spring.data.redis.cluster.nodes entry '" + node + "' has a port outside 1..65535");
            }
            seeds.add(rateLimitRedisUri(node.substring(0, colon), port, password));
        }
        return List.copyOf(seeds);
    }

    /**
     * 限流专用连接的客户端选项(单实例)。包内可见,供单测钉住。
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

    /**
     * 集群形态的客户端选项:断线、超时、在途上限三项与 {@link #rateLimitClientOptions()} 相同;另开拓扑刷新 ——
     * 自适应刷新(收到 MOVED / ASK、持续重连时立即刷新)加 {@link #CLUSTER_TOPOLOGY_REFRESH_PERIOD} 的定期刷新。
     * 不开的话,迁槽或换主之后每条命令都要多走一次重定向,换主之后还会持续打到已经下线的旧主。包内可见,供单测钉住。
     */
    static ClusterClientOptions rateLimitClusterClientOptions() {
        return ClusterClientOptions.builder()
                .disconnectedBehavior(ClientOptions.DisconnectedBehavior.REJECT_COMMANDS)
                .timeoutOptions(TimeoutOptions.enabled(REDIS_COMMAND_TIMEOUT))
                .requestQueueSize(REDIS_REQUEST_QUEUE_SIZE)
                .topologyRefreshOptions(ClusterTopologyRefreshOptions.builder()
                        .enableAllAdaptiveRefreshTriggers()
                        .enablePeriodicRefresh(CLUSTER_TOPOLOGY_REFRESH_PERIOD)
                        .build())
                .build();
    }

    /** 限流专用的那一条连接;连不上直接抛异常,Spring 随之拒启。 */
    @Bean(destroyMethod = "close")
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public RateLimitRedis rateLimitRedis(AbstractRedisClient rateLimitRedisClient) {
        return RateLimitRedis.connect(rateLimitRedisClient);
    }

    @Bean
    @ConditionalOnProperty(prefix = "gate.rate-limit", name = "enabled", havingValue = "true")
    public RateLimitStore rateLimitStore(RateLimitRedis redis) {
        return new RedisRateLimitStore(redis.commands());
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
