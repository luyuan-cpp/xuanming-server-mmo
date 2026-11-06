package com.game.gateway.ratelimit;

import com.game.gateway.ratelimit.RateLimitStore.Admission;
import com.game.gateway.ratelimit.RateLimitStore.Bucket;
import io.lettuce.core.RedisCommandTimeoutException;
import io.lettuce.core.RedisNoScriptException;
import io.lettuce.core.ScriptOutputType;
import io.lettuce.core.api.sync.RedisCommands;
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
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link RedisRateLimitStore} 的 Java 侧:调用参数的编码、回包解析、NOSCRIPT 回退。不连 Redis(Lettuce 同步接口用 Mockito 代替);
 * 脚本本身的令牌桶算术见 {@link RedisRateLimitStoreIntegrationTest}。
 */
class RedisRateLimitStoreTest {

    private static final String SHA = "0123456789abcdef0123456789abcdef01234567";

    private RedisCommands<byte[], byte[]> redis;
    private RedisRateLimitStore store;

    @BeforeEach
    @SuppressWarnings("unchecked")
    void setUp() {
        redis = mock(RedisCommands.class);
        when(redis.digest(anyString())).thenReturn(SHA);
        store = new RedisRateLimitStore(redis);
    }

    private void evalshaReplies(List<Object> reply) {
        when(redis.evalsha(eq(SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenReturn(reply);
    }

    private static String text(byte[] bytes) {
        return new String(bytes, StandardCharsets.UTF_8);
    }

    @Test
    void ipAndZone_sentAsTwoKeysAndFiveArgs() {
        evalshaReplies(List.of(0L));

        Admission a = store.admitAt(1_700_000_000_123L, new Bucket("rl:ip:1.2.3.4", 5, 20), new Bucket("rl:zone:7", 2000, 5000));

        assertEquals(Admission.Kind.OK, a.kind());
        ArgumentCaptor<byte[][]> keys = ArgumentCaptor.forClass(byte[][].class);
        ArgumentCaptor<byte[][]> args = ArgumentCaptor.forClass(byte[][].class);
        verify(redis).evalsha(eq(SHA), eq(ScriptOutputType.MULTI), keys.capture(), args.capture());
        assertArrayEquals(new String[] {"rl:ip:1.2.3.4", "rl:zone:7"},
                Arrays.stream(keys.getValue()).map(RedisRateLimitStoreTest::text).toArray(String[]::new));
        assertArrayEquals(new String[] {"1700000000123", "5", "20", "2000", "5000"},
                Arrays.stream(args.getValue()).map(RedisRateLimitStoreTest::text).toArray(String[]::new),
                "ARGV 顺序:now_ms, ip_rps, ip_burst, zone_rps, zone_burst");
    }

    @Test
    void ipOnly_sentAsOneKeyAndServerTimeMarker() {
        evalshaReplies(List.of(0L));

        store.admit(new Bucket("rl:ip:?", 2, 10), null);

        ArgumentCaptor<byte[][]> keys = ArgumentCaptor.forClass(byte[][].class);
        ArgumentCaptor<byte[][]> args = ArgumentCaptor.forClass(byte[][].class);
        verify(redis).evalsha(eq(SHA), eq(ScriptOutputType.MULTI), keys.capture(), args.capture());
        assertEquals(1, keys.getValue().length, "zone = null 只传 IP 桶一个键");
        assertEquals(3, args.getValue().length);
        assertEquals(0, args.getValue()[0].length, "生产路径 ARGV[1] 为空串:脚本取 Redis 服务器的 TIME");
    }

    @Test
    void replies_mapToAdmissions() {
        Bucket ip = new Bucket("rl:ip:a", 1, 1);
        Bucket zone = new Bucket("rl:zone:1", 1, 1);

        evalshaReplies(List.of(1L));
        assertEquals(Admission.ipEmpty(), store.admit(ip, zone));

        evalshaReplies(List.of(2L, 250L));
        assertEquals(Admission.zoneEmpty(250), store.admit(ip, zone), "{2, wait_ms} → 区桶空,带等待毫秒");

        evalshaReplies(List.of(9L));
        assertThrows(IllegalStateException.class, () -> store.admit(ip, zone), "未知结果不能当放行");
    }

    @Test
    void noScript_fallsBackToEvalWithFullScript() {
        // Redis 重启或脚本缓存被清后,EVALSHA 回 NOSCRIPT:带全文 EVAL 一次。
        when(redis.evalsha(eq(SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenThrow(new RedisNoScriptException("NOSCRIPT No matching script. Please use EVAL."));
        when(redis.eval(eq(RedisRateLimitStore.ADMIT_SCRIPT), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenReturn(List.of(1L));

        assertEquals(Admission.ipEmpty(), store.admit(new Bucket("rl:ip:b", 1, 1), null));
        verify(redis).eval(eq(RedisRateLimitStore.ADMIT_SCRIPT), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class));
    }

    @Test
    void otherErrors_propagateWithoutEval() {
        // 超时、断线等其它错误原样抛给限流器(由它 fail-open),不重发 EVAL。
        when(redis.evalsha(eq(SHA), eq(ScriptOutputType.MULTI), any(byte[][].class), any(byte[][].class)))
                .thenThrow(new RedisCommandTimeoutException("timeout"));

        assertThrows(RedisCommandTimeoutException.class,
                () -> store.admit(new Bucket("rl:ip:c", 1, 1), null));
        verify(redis, never()).eval(anyString(), any(ScriptOutputType.class), any(byte[][].class), any(byte[][].class));
    }
}
