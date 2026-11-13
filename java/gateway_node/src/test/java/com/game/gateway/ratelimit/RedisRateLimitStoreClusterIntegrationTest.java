package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.lettuce.core.cluster.RedisClusterClient;
import io.lettuce.core.cluster.SlotHash;
import io.lettuce.core.cluster.api.StatefulRedisClusterConnection;
import io.lettuce.core.cluster.api.sync.RedisAdvancedClusterCommands;
import io.lettuce.core.codec.ByteArrayCodec;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.UUID;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;

/**
 * 限流在 Redis Cluster 上(2026-11-12,docs/design/redis-cluster-all-domains-20261110.md §4.5):IP 桶与区桶落在不同槽时,
 * 每个桶一段单键脚本照样判得对,区桶空时退还 IP 令牌。以前两个桶在同一段 Lua 里,这种形状会收到 CROSSSLOT。
 *
 * <p>只在设置了 {@code GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES}(集群种子,逗号分隔,如
 * {@code 127.0.0.1:7100,127.0.0.1:7101,127.0.0.1:7102},即 deploy/compose.redis-cluster.yml)时运行。客户端按生产的装配建
 * ({@link RateLimitConfig#rateLimitClusterSeeds}、{@link RateLimitConfig#rateLimitClusterClientOptions()}、
 * {@link RateLimitRedis#connect})。每个用例用随机前缀的键,结束时逐个删掉。
 */
@EnabledIfEnvironmentVariable(named = "GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES", matches = ".+")
class RedisRateLimitStoreClusterIntegrationTest {

    private static final long T = 1_800_000_000_123L;
    private static final long HOLD_MS = 60_000;

    private static RedisClusterClient client;
    private static RateLimitRedis rateLimitRedis;
    private static StatefulRedisClusterConnection<byte[], byte[]> inspect;

    private RedisAdvancedClusterCommands<byte[], byte[]> redis;
    private RedisRateLimitStore store;
    private final List<String> used = new ArrayList<>();

    @BeforeAll
    static void connect() {
        List<String> nodes = Arrays.asList(System.getenv("GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES").split(","));
        client = RedisClusterClient.create(RateLimitConfig.rateLimitClusterSeeds(nodes, System.getenv("GATEWAY_RATE_LIMIT_REDIS_PASSWORD")));
        client.setOptions(RateLimitConfig.rateLimitClusterClientOptions());
        rateLimitRedis = RateLimitRedis.connect(client);
        inspect = client.connect(ByteArrayCodec.INSTANCE);
    }

    @AfterAll
    static void disconnect() {
        inspect.close();
        rateLimitRedis.close();
        client.shutdown();
    }

    @BeforeEach
    void setUp() {
        redis = inspect.sync();
        store = new RedisRateLimitStore(rateLimitRedis.commands());
    }

    @AfterEach
    void cleanUp() {
        for (String key : used) {
            redis.del(utf8(key));
        }
    }

    /** 一对落在不同槽上的 IP 桶键与区桶键。 */
    private Bucket[] bucketsOnDifferentSlots(long ipRps, long ipBurst, long zoneRps, long zoneBurst) {
        String prefix = "rl:it:" + UUID.randomUUID() + ":";
        String ipKey = prefix + "ip";
        String zoneKey = null;
        for (int i = 0; zoneKey == null; i++) {
            String candidate = prefix + "zone:" + i;
            if (SlotHash.getSlot(candidate) != SlotHash.getSlot(ipKey)) {
                zoneKey = candidate;
            }
        }
        used.add(ipKey);
        used.add(zoneKey);
        return new Bucket[] {new Bucket(ipKey, ipRps, ipBurst), new Bucket(zoneKey, zoneRps, zoneBurst)};
    }

    private static byte[] utf8(String s) {
        return s.getBytes(StandardCharsets.UTF_8);
    }

    @Test
    void ok_takesOneTokenFromEachBucketOnDifferentSlots() {
        Bucket[] b = bucketsOnDifferentSlots(1, 5, 1, 5);
        assertNotEquals(SlotHash.getSlot(b[0].key()), SlotHash.getSlot(b[1].key()), "前提:两个桶不同槽");

        assertEquals(Admission.ok(), store.admitAt(T, b[0], b[1]));
        assertEquals("4000", new String(redis.hget(utf8(b[0].key()), utf8("t")), StandardCharsets.UTF_8));
        assertEquals("4000", new String(redis.hget(utf8(b[1].key()), utf8("t")), StandardCharsets.UTF_8));
    }

    @Test
    void zoneEmpty_refundsTheIpTokenOnItsOwnNode() {
        Bucket[] b = bucketsOnDifferentSlots(1, 2, 1, 1);
        // 先把区桶扣空(只判这一个桶的入口没有,借一个满的 IP 桶扣一次)。
        assertEquals(Admission.ok(), store.admitAt(T, new Bucket(b[0].key() + ":warm", 1, 5), b[1]));
        used.add(b[0].key() + ":warm");
        // 区桶键按补满时刻设了真实 TTL(1 秒),慢机器上可能先过期、变回满桶;拉长到一分钟。
        redis.pexpire(utf8(b[1].key()), HOLD_MS);

        assertEquals(Admission.zoneEmpty(1000), store.admitAt(T, b[0], b[1]), "区桶空:等补到一个令牌");
        assertEquals(0L, redis.exists(utf8(b[0].key())), "满的 IP 桶扣一个再还回去还是满桶,键已删");
    }

    @Test
    void ipEmpty_doesNotTouchTheZoneBucket() {
        Bucket[] b = bucketsOnDifferentSlots(1, 1, 1, 1);
        assertEquals(Admission.ok(), store.admitAt(T, b[0], null));
        redis.pexpire(utf8(b[0].key()), HOLD_MS);

        assertEquals(Admission.ipEmpty(), store.admitAt(T, b[0], b[1]));
        assertEquals(0L, redis.exists(utf8(b[1].key())), "IP 桶空时区桶不读不写");
    }
}
