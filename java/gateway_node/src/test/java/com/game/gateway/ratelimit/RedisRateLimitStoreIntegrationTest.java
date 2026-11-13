package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.lettuce.core.RedisClient;
import io.lettuce.core.ScriptOutputType;
import io.lettuce.core.api.StatefulRedisConnection;
import io.lettuce.core.api.sync.RedisCommands;
import io.lettuce.core.codec.ByteArrayCodec;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;

import java.math.BigDecimal;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 令牌桶 Lua({@link RedisRateLimitStore#TAKE_SCRIPT}、{@link RedisRateLimitStore#REFUND_SCRIPT})对真实 Redis 的行为
 * (2026-11-05,服务器缺口清单第 29 条;2026-11-12 起每个桶一段单键脚本,区桶空时退还 IP 令牌)。
 * 集群形态(两个桶在不同槽)见 {@link RedisRateLimitStoreClusterIntegrationTest}。
 *
 * <p>只在设置了 {@code GATEWAY_RATE_LIMIT_REDIS_URL}(如 {@code redis://127.0.0.1:6390})时运行,指向一个一次性的
 * Redis 容器;K8s 钉的 7.2 与本地 compose 的 8.x 各跑一轮。每个用例用随机前缀的 {@code rl:it:*} 键,结束时删掉。
 *
 * <p>时间:除最后一例外都用 {@code admitAt} 指定脚本里的「当前毫秒」。键的过期却按 Redis 的真实时钟走(放行后 TTL 只有
 * 补满所需的那几百毫秒到几秒),所以凡是后面的断言依赖桶键还在的,都先用 {@link #seed} 直接写入桶状态、或用
 * {@link #holdTtl} 把真实 TTL 拉长到一分钟,免得慢机器上键先按真实时间过期、用例随机失败。
 */
@EnabledIfEnvironmentVariable(named = "GATEWAY_RATE_LIMIT_REDIS_URL", matches = ".+")
class RedisRateLimitStoreIntegrationTest {

    private static final long T = 1_800_000_000_123L;
    private static final long HOLD_MS = 60_000;

    private static RedisClient client;
    private static StatefulRedisConnection<byte[], byte[]> connection;

    private RedisCommands<byte[], byte[]> redis;
    private RedisRateLimitStore store;
    private String prefix;

    @BeforeAll
    static void connect() {
        client = RedisClient.create(System.getenv("GATEWAY_RATE_LIMIT_REDIS_URL"));
        connection = client.connect(ByteArrayCodec.INSTANCE);
    }

    @AfterAll
    static void disconnect() {
        connection.close();
        client.shutdown();
    }

    @BeforeEach
    void setUp() {
        redis = connection.sync();
        store = new RedisRateLimitStore(redis);
        prefix = "rl:it:" + UUID.randomUUID() + ":";
    }

    @AfterEach
    void cleanUp() {
        List<byte[]> keys = redis.keys(utf8(prefix + "*"));
        if (!keys.isEmpty()) {
            redis.del(keys.toArray(new byte[0][]));
        }
    }

    private Bucket bucket(String name, long rps, long burst) {
        return new Bucket(prefix + name, rps, burst);
    }

    /** 直接写入桶状态:余量 {@code milliTokens}(千分之一令牌)对应时刻 {@code ms},真实 TTL 一分钟。 */
    private void seed(Bucket b, long milliTokens, long ms) {
        redis.hset(utf8(b.key()), Map.of(utf8("t"), utf8(Long.toString(milliTokens)), utf8("ms"), utf8(Long.toString(ms))));
        holdTtl(b);
    }

    private void holdTtl(Bucket b) {
        redis.pexpire(utf8(b.key()), HOLD_MS);
    }

    /**
     * 桶状态 [t, ms],不存在的字段为 null。Redis 把 Lua 数字转成字符串时可能写成科学计数法(如 1.8e+12),
     * 脚本里的 tonumber 读得回原值;这里同样按数值解析,不比字符串。
     */
    private List<Long> state(Bucket b) {
        return redis.hmget(utf8(b.key()), utf8("t"), utf8("ms")).stream()
                .map(kv -> kv.hasValue() ? new BigDecimal(new String(kv.getValue(), StandardCharsets.UTF_8)).longValueExact() : null)
                .toList();
    }

    private static byte[] utf8(String s) {
        return s.getBytes(StandardCharsets.UTF_8);
    }

    @Test
    void freshBucketIsFull() {
        Bucket ip = bucket("ip", 1, 3);

        for (int i = 0; i < 3; i++) {
            assertEquals(Admission.ok(), store.admitAt(T, ip, null), "键不存在 = 满桶,burst = 3 能连放 3 个");
            holdTtl(ip);
        }
        assertEquals(Admission.ipEmpty(), store.admitAt(T, ip, null));
    }

    @Test
    void emptyBucket_refillsAtRps() {
        Bucket ip = bucket("ip", 1, 3);
        seed(ip, 0, T);

        assertEquals(Admission.ipEmpty(), store.admitAt(T + 999, ip, null), "每秒补 1 个:999ms 只补了 0.999 个");
        assertEquals(Admission.ok(), store.admitAt(T + 1000, ip, null), "满 1 秒补够一个");
        holdTtl(ip);
        assertEquals(Admission.ipEmpty(), store.admitAt(T + 1000, ip, null));
        for (int i = 0; i < 3; i++) {
            assertEquals(Admission.ok(), store.admitAt(T + 4000, ip, null), "空闲 3 秒补满到 burst = 3,补充量封顶");
            holdTtl(ip);
        }
        assertEquals(Admission.ipEmpty(), store.admitAt(T + 4000, ip, null));
    }

    @Test
    void take_writesRemainingTokensAndTime_rejectionWritesNothing() {
        Bucket ip = bucket("ip", 1, 2);
        assertEquals(Admission.ok(), store.admitAt(T, ip, null));
        holdTtl(ip);
        assertEquals(Arrays.asList(1000L, T), state(ip), "放行写回扣后的余量(千分之一令牌)与时刻");

        assertEquals(Admission.ok(), store.admitAt(T, ip, null));
        holdTtl(ip);
        assertEquals(Arrays.asList(0L, T), state(ip));

        assertEquals(Admission.ipEmpty(), store.admitAt(T + 500, ip, null));
        assertEquals(Arrays.asList(0L, T), state(ip), "拒绝路径只读:补充量由 (t, ms) 与当前时刻算出,不写回");
    }

    @Test
    void zoneEmpty_returnsCeilWait_andDoesNotSpendIpToken() {
        Bucket ip = bucket("ip", 1, 2);
        Bucket zone = bucket("zone", 3, 1);
        seed(ip, 1000, T);
        seed(zone, 0, T);

        // 100ms 后区桶补了 0.3 个:还差 0.7 个,每秒 3 个 → 233.3ms,向上取整 234。
        assertEquals(Admission.zoneEmpty(234), store.admitAt(T + 100, ip, zone));
        // 退还时按补满时刻设了真实 TTL(这里不到一秒),后面还要用这个桶,先拉长。
        holdTtl(ip);
        // IP 桶先扣后还:状态记到这一次的时刻,余量与从没扣过时相同(1 + 0.1 个)。
        assertEquals(Arrays.asList(1100L, T + 100), state(ip), "区桶空时 IP 令牌已退还,余量不变");

        // 此刻 IP 余量 1 + 0.1 = 1.1 个:只判 IP 还能放一个,之后才空。
        assertEquals(Admission.ok(), store.admitAt(T + 100, ip, null));
        holdTtl(ip);
        assertEquals(Admission.ipEmpty(), store.admitAt(T + 100, ip, null));
    }

    @Test
    void zoneEmpty_onAFullIpBucket_leavesNoIpKey() {
        Bucket ip = bucket("ip", 1, 2);
        Bucket zone = bucket("zone", 1, 1);
        seed(zone, 0, T);

        assertEquals(Admission.zoneEmpty(1000), store.admitAt(T, ip, zone), "区桶差一个令牌、每秒补 1 个:等 1000ms");
        assertEquals(0L, redis.exists(utf8(ip.key())),
                "满桶扣一个再还回去还是满桶:键删掉(键不存在 = 满桶),不留一个按补满时刻才过期的键");
    }

    @Test
    void refund_ofAMissingKeyDoesNothing() {
        Bucket ip = bucket("ip", 1, 2);
        Long reply = redis.eval(RedisRateLimitStore.REFUND_SCRIPT, ScriptOutputType.INTEGER,
                new byte[][] {utf8(ip.key())}, utf8(Long.toString(T)), utf8("1"), utf8("2"));

        assertEquals(0L, reply, "键不存在 = 已是满桶,没有可还的");
        assertEquals(0L, redis.exists(utf8(ip.key())), "退还不凭空建出桶键");
    }

    @Test
    void ipEmpty_doesNotTouchZone() {
        Bucket ip = bucket("ip", 1, 1);
        Bucket zone = bucket("zone", 1, 1);
        seed(ip, 0, T);

        assertEquals(Admission.ipEmpty(), store.admitAt(T, ip, zone));
        assertEquals(0L, redis.exists(utf8(zone.key())), "IP 桶空时区桶不读不写");
        assertEquals(Admission.ok(), store.admitAt(T, bucket("other-ip", 1, 1), zone), "区桶仍是满的");
    }

    @Test
    void ok_takesOneTokenFromEachBucket() {
        Bucket ip = bucket("ip", 1, 5);
        Bucket zone = bucket("zone", 1, 5);

        assertEquals(Admission.ok(), store.admitAt(T, ip, zone));
        holdTtl(ip);
        holdTtl(zone);
        assertEquals(Arrays.asList(4000L, T), state(ip), "放行时两个桶各扣一个");
        assertEquals(Arrays.asList(4000L, T), state(zone));
    }

    @Test
    void keyExpiresWhenBucketWouldBeFull() {
        Bucket ip = bucket("ip", 1, 10);

        assertEquals(Admission.ok(), store.admitAt(T, ip, null));
        long ttl = redis.pttl(utf8(ip.key()));
        assertTrue(ttl > 0 && ttl <= 1000, "差 1 个令牌、每秒补 1 个:1 秒后补满,键随之过期;实际 PTTL " + ttl);

        assertEquals(Admission.ok(), store.admitAt(T, ip, null));
        ttl = redis.pttl(utf8(ip.key()));
        assertTrue(ttl > 1000 && ttl <= 2000, "差 2 个令牌:2 秒后补满;实际 PTTL " + ttl);
    }

    @Test
    void newThresholds_applyToExistingBucketImmediately() {
        assertEquals(Admission.ok(), store.admitAt(T, bucket("ip", 1, 10), null), "旧阈值下剩 9 个");
        holdTtl(bucket("ip", 1, 10));

        // 滚动 gateway 把 burst 改成 2:余量按新 burst 封顶为 2 个,同一时刻只能再放 2 个。
        Bucket shrunk = bucket("ip", 1, 2);
        assertEquals(Admission.ok(), store.admitAt(T, shrunk, null));
        holdTtl(shrunk);
        assertEquals(Admission.ok(), store.admitAt(T, shrunk, null));
        holdTtl(shrunk);
        assertEquals(Admission.ipEmpty(), store.admitAt(T, shrunk, null));
    }

    @Test
    void serverTime_isUsedWhenNoTimeGiven() {
        // 生产路径:脚本先调 TIME 再写(Redis 5 起缺省按效果复制,允许这样写)。写下的 ms 应接近当前时刻。
        Bucket ip = bucket("ip", 1, 2);
        Bucket zone = bucket("zone", 1, 2);
        assertEquals(Admission.ok(), store.admit(ip, zone));
        holdTtl(zone);

        long written = state(zone).get(1);
        assertTrue(Math.abs(written - System.currentTimeMillis()) < 60_000,
                "ms 应取 Redis 服务器时间(本机容器,与测试进程时钟相差不到一分钟);实际 " + written);
    }
}
