package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.lettuce.core.RedisCommandTimeoutException;
import io.lettuce.core.RedisNoScriptException;
import io.lettuce.core.ScriptOutputType;
import io.lettuce.core.api.sync.RedisScriptingCommands;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link RedisRateLimitStore} 的 Java 侧:两段单键脚本的调用顺序与参数、回包解析、退还、NOSCRIPT 回退。不连 Redis
 * (Lettuce 同步接口用 Mockito 代替);脚本本身的令牌桶算术见 {@link RedisRateLimitStoreIntegrationTest}。
 */
class RedisRateLimitStoreTest {

    private static final String TAKE_SHA = "0123456789abcdef0123456789abcdef01234567";
    private static final String REFUND_SHA = "fedcba9876543210fedcba9876543210fedcba98";

    private static final Bucket IP = new Bucket("rl:ip:1.2.3.4", 5, 20);
    private static final Bucket ZONE = new Bucket("rl:zone:7", 2000, 5000);

    private RedisScriptingCommands<byte[], byte[]> redis;
    private RedisRateLimitStore store;

    @BeforeEach
    @SuppressWarnings("unchecked")
    void setUp() {
        redis = mock(RedisScriptingCommands.class);
        when(redis.digest(RedisRateLimitStore.TAKE_SCRIPT)).thenReturn(TAKE_SHA);
        when(redis.digest(RedisRateLimitStore.REFUND_SCRIPT)).thenReturn(REFUND_SHA);
        store = new RedisRateLimitStore(redis);
    }

    /** 每个桶的扣令牌结果:按调用顺序依次返回。 */
    private void takeReplies(List<?> first, List<?>... rest) {
        when(redis.evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenReturn(first, (Object[]) rest);
    }

    private static String text(byte[] bytes) {
        return new String(bytes, StandardCharsets.UTF_8);
    }

    private static String[] texts(byte[][] values) {
        return Arrays.stream(values).map(RedisRateLimitStoreTest::text).toArray(String[]::new);
    }

    @Test
    void ipAndZone_eachBucketIsOneKeyAndThreeArgs_ipFirst() {
        takeReplies(List.of(0L, 0L), List.of(0L, 0L));

        Admission a = store.admitAt(1_700_000_000_123L, IP, ZONE);

        assertEquals(Admission.Kind.OK, a.kind());
        ArgumentCaptor<byte[][]> keys = ArgumentCaptor.forClass(byte[][].class);
        ArgumentCaptor<byte[][]> args = ArgumentCaptor.forClass(byte[][].class);
        verify(redis, times(2)).evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), keys.capture(), args.capture());
        assertArrayEquals(new String[] {"rl:ip:1.2.3.4"}, texts(keys.getAllValues().get(0)), "第一段只碰 IP 桶");
        assertArrayEquals(new String[] {"1700000000123", "5", "20"}, texts(args.getAllValues().get(0)),
                "ARGV 顺序:now_ms, rps, burst");
        assertArrayEquals(new String[] {"rl:zone:7"}, texts(keys.getAllValues().get(1)), "第二段只碰区桶:两段各一个键,集群上可以不同槽");
        assertArrayEquals(new String[] {"1700000000123", "2000", "5000"}, texts(args.getAllValues().get(1)));
        verify(redis, never()).evalsha(eq(REFUND_SHA), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class));
    }

    @Test
    void ipOnly_oneScriptWithServerTimeMarker() {
        takeReplies(List.of(0L, 0L));

        assertEquals(Admission.ok(), store.admit(new Bucket("rl:ip:?", 2, 10), null));

        ArgumentCaptor<byte[][]> keys = ArgumentCaptor.forClass(byte[][].class);
        ArgumentCaptor<byte[][]> args = ArgumentCaptor.forClass(byte[][].class);
        verify(redis).evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), keys.capture(), args.capture());
        assertEquals(1, keys.getValue().length, "zone = null 只扣 IP 桶");
        assertEquals(3, args.getValue().length);
        assertEquals(0, args.getValue()[0].length, "生产路径 ARGV[1] 为空串:脚本取 Redis 服务器的 TIME");
    }

    @Test
    void ipEmpty_neverTouchesTheZoneBucket() {
        takeReplies(List.of(1L, 200L));

        assertEquals(Admission.ipEmpty(), store.admit(IP, ZONE));

        verify(redis, times(1)).evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class));
        verify(redis, never()).evalsha(eq(REFUND_SHA), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class));
    }

    @Test
    void zoneEmpty_refundsTheIpTokenAndCarriesTheZoneWait() {
        takeReplies(List.of(0L, 0L), List.of(1L, 250L));
        when(redis.evalsha(eq(REFUND_SHA), eq(ScriptOutputType.INTEGER), any(byte[][].class), any(byte[][].class))).thenReturn(1L);

        assertEquals(Admission.zoneEmpty(250), store.admitAt(1_700_000_000_123L, IP, ZONE), "{1, wait_ms} → 区桶空,带等待毫秒");

        ArgumentCaptor<byte[][]> keys = ArgumentCaptor.forClass(byte[][].class);
        ArgumentCaptor<byte[][]> args = ArgumentCaptor.forClass(byte[][].class);
        verify(redis).evalsha(eq(REFUND_SHA), eq(ScriptOutputType.INTEGER), keys.capture(), args.capture());
        assertArrayEquals(new String[] {"rl:ip:1.2.3.4"}, texts(keys.getValue()), "退还的是 IP 桶");
        assertArrayEquals(new String[] {"1700000000123", "5", "20"}, texts(args.getValue()), "退还按同一时刻与 IP 桶的阈值算");
    }

    @Test
    void zoneEmpty_refundFailureStillAnswersZoneEmpty() {
        // 退还失败只是 IP 桶多扣了一个(偏严);区桶确实空了,不能把这次判定抛给限流器去 fail-open 放行。
        takeReplies(List.of(0L, 0L), List.of(1L, 40L));
        when(redis.evalsha(eq(REFUND_SHA), eq(ScriptOutputType.INTEGER), any(byte[][].class), any(byte[][].class)))
                .thenThrow(new RedisCommandTimeoutException("timeout"));

        assertEquals(Admission.zoneEmpty(40), store.admit(IP, ZONE));
    }

    @Test
    void zoneTakeError_propagatesForFailOpen() {
        // 区桶那一条出错:异常照常交给限流器(fail-open 放行,IP 令牌已扣,与放行一致),不去退还。
        when(redis.evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenReturn(List.of(0L, 0L))
                .thenThrow(new RedisCommandTimeoutException("timeout"));

        assertThrows(RedisCommandTimeoutException.class, () -> store.admit(IP, ZONE));
        verify(redis, never()).evalsha(eq(REFUND_SHA), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class));
    }

    @Test
    void unknownReplies_areRejected() {
        for (List<?> reply : List.of(List.of(9L, 0L), List.of(0L), List.of("x", 0L))) {
            setUp();
            takeReplies(reply);
            assertThrows(IllegalStateException.class, () -> store.admit(IP, null), "未知结果不能当放行: " + reply);
        }
    }

    @Test
    void noScript_fallsBackToEvalWithFullScript_forBothScripts() {
        // Redis 重启、脚本缓存被清,或集群里第一次落到某个节点:EVALSHA 回 NOSCRIPT,带全文 EVAL 一次。
        when(redis.evalsha(anyString(), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class)))
                .thenThrow(new RedisNoScriptException("NOSCRIPT No matching script. Please use EVAL."));
        when(redis.eval(eq(RedisRateLimitStore.TAKE_SCRIPT), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenReturn(List.of(0L, 0L), List.of(1L, 7L));
        when(redis.eval(eq(RedisRateLimitStore.REFUND_SCRIPT), eq(ScriptOutputType.INTEGER), any(byte[][].class), any(byte[][].class)))
                .thenReturn(1L);

        assertEquals(Admission.zoneEmpty(7), store.admit(IP, ZONE));
        verify(redis, times(2)).eval(eq(RedisRateLimitStore.TAKE_SCRIPT), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class));
        verify(redis).eval(eq(RedisRateLimitStore.REFUND_SCRIPT), eq(ScriptOutputType.INTEGER), any(byte[][].class), any(byte[][].class));
    }

    @Test
    void otherErrors_propagateWithoutEval() {
        // 超时、断线等其它错误原样抛给限流器(由它 fail-open),不重发 EVAL。
        when(redis.evalsha(eq(TAKE_SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenThrow(new RedisCommandTimeoutException("timeout"));

        assertThrows(RedisCommandTimeoutException.class, () -> store.admit(new Bucket("rl:ip:c", 1, 1), null));
        verify(redis, never()).eval(anyString(), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class));
    }
}
