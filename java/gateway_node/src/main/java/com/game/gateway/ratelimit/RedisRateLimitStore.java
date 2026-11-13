package com.game.gateway.ratelimit;

import io.lettuce.core.RedisNoScriptException;
import io.lettuce.core.ScriptOutputType;
import io.lettuce.core.api.sync.RedisScriptingCommands;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.nio.charset.StandardCharsets;
import java.util.List;

/**
 * {@link RateLimitStore} 的 Redis 实现(2026-11-05,服务器缺口清单第 29 条,替代原 Bucket4j 的 CAS 后端)。
 *
 * <p><b>为什么换掉 Bucket4j。</b>Bucket4j 的 Lettuce 后端是「读状态 → 本地算 → CAS 写回,冲突就重试」。区桶
 * {@code rl:zone:<id>} 是全区请求共用的热键,副本越多 CAS 冲突越多,重试把一次桶操作拖过时限就按异常放行 ——
 * 开服洪峰正是最该限流的时候,它却最容易漏。这里改成 Lua:Redis 单线程执行一段脚本,没有冲突也没有重试。
 *
 * <p><b>桶的形状。</b>Hash:{@code t} = 余量(以千分之一令牌为单位的整数,免得浮点误差累积),{@code ms} = 这份余量
 * 对应的 Redis 时刻(毫秒)。键不存在 = 满桶。每秒补 rps 个令牌 = 每毫秒补 rps 个千分之一令牌,补充量只取决于经过的时间,所以:
 * <ul>
 *   <li>拒绝路径只读:不扣令牌就不写回,下一次照样从 (t, ms) 算出同样的余量 —— 洪峰时大量被拒的请求不产生写;</li>
 *   <li>放行时写回扣后的余量,并让键在「补满的那一刻」过期:过期后「键不存在 = 满桶」与不过期时算出的余量完全相同,
 *       所以不需要另设闲置 TTL,一次性 IP 的桶几秒内就回收(原 Bucket4j 要留「补满时间 + 1 小时」)。</li>
 * </ul>
 * rps / burst 每次随调用传进脚本:改阈值后滚动 gateway,已有的桶立即按新阈值算(余量按新 burst 封顶),不必清桶。
 *
 * <p><b>一个桶一段脚本(2026-11-12,docs/design/redis-cluster-all-domains-20261110.md §4.5)。</b>以前 IP 桶与区桶在同一段
 * Lua 里一次判完,两个键必须在同一个实例上;上了 Redis Cluster 两键不同槽,那段脚本会报 CROSSSLOT。现在每段脚本只碰一个键:
 * <ol>
 *   <li>扣 IP 桶({@link #TAKE_SCRIPT}):不足一个令牌 → IP_EMPTY,什么都没写,区桶不碰;</li>
 *   <li>没有区桶(zone_id = 0)→ OK;</li>
 *   <li>扣区桶:够 → OK;</li>
 *   <li>区桶不足一个令牌 → 把第 1 步扣的那个 IP 令牌还回去({@link #REFUND_SCRIPT}),回 ZONE_EMPTY 与区桶的等待毫秒。</li>
 * </ol>
 * 判定的顺序与结局和以前相同(先 IP 后区,区桶空时 IP 令牌不算扣),差别只在「不再是一个原子操作」:
 * <ul>
 *   <li>第 1 步到第 4 步之间,别的请求看到的 IP 桶少一个令牌,偏严;</li>
 *   <li>还回去的余量不超过「从没扣过」时的余量(两者只在补满封顶时有差,而且是还回去的那一边少),不会因为退还而多放;</li>
 *   <li>退还这一条失败(超时、断线)时 IP 桶多扣了一个令牌,偏严;这次判定照样回 ZONE_EMPTY(区桶确实空了),只打 WARN,
 *       不抛给限流器 —— 抛出去会被当成 Redis 故障、放行这个请求并熔断五秒;</li>
 *   <li>第 3 步出错时异常照常抛给限流器,按 fail-open 放行;IP 令牌已扣,与「放行」的结局一致。</li>
 * </ul>
 * 不给 IP 桶按 zone 打 tag:那样每个 IP 在每个 zone 各有一个桶,语义就变了。
 *
 * <p><b>时钟。</b>取 Redis 的 TIME。集群上两个桶可能在两个节点上,各用各的节点时钟;一个桶的读写只在它自己的节点上,
 * 补充量只看同一个时钟的差,节点之间的偏差不影响。Redis 所在机器的时钟往回跳时按经过 0 毫秒算、不补令牌,
 * 最多卡住回跳的那么久;下一次放行把 ms 写成新时刻,之后恢复正常。
 *
 * <p><b>超时与重发。</b>命令超时由专用 Lettuce 连接约束({@link RateLimitConfig#REDIS_COMMAND_TIMEOUT})。Lettuce
 * 不重发超时的命令;超时的那一次若其实已在 Redis 上执行,就多扣了一个令牌(偏严,可接受)。集群客户端跟随的 MOVED / ASK
 * 是「没执行」的回复,跟随重发不会重复扣。
 *
 * <p>线程安全(Lettuce 同步接口可多线程共用一条连接);阻塞,只在请求线程上调用。
 */
public final class RedisRateLimitStore implements RateLimitStore {

    private static final Logger log = LoggerFactory.getLogger(RedisRateLimitStore.class);

    /**
     * 扣一个令牌。KEYS:桶;ARGV:now_ms(空串 = 取 Redis 服务器的 TIME), rps, burst。
     * 返回 {0, 0} 已扣 / {1, wait_ms} 不足一个令牌、什么都没写(wait_ms = 补到一个令牌还要等的毫秒,向上取整)。
     * 语义见类注释,脚本正文不写注释。
     */
    static final String TAKE_SCRIPT = """
            local now
            if ARGV[1] == '' then
              local tm = redis.call('TIME')
              now = tonumber(tm[1]) * 1000 + math.floor(tonumber(tm[2]) / 1000)
            else
              now = tonumber(ARGV[1])
            end
            local rps, burst = tonumber(ARGV[2]), tonumber(ARGV[3]) * 1000
            local state = redis.call('HMGET', KEYS[1], 't', 'ms')
            local t, last = tonumber(state[1]), tonumber(state[2])
            local level = burst
            if t ~= nil and last ~= nil then
              level = math.min(burst, t + math.max(0, now - last) * rps)
            end
            if level < 1000 then
              return {1, math.ceil((1000 - level) / rps)}
            end
            local left = level - 1000
            redis.call('HSET', KEYS[1], 't', left, 'ms', now)
            redis.call('PEXPIRE', KEYS[1], math.ceil((burst - left) / rps))
            return {0, 0}
            """;

    /**
     * 还回一个令牌(扣过 IP 桶、区桶却空了时)。KEYS:桶;ARGV 同 {@link #TAKE_SCRIPT}。
     * 键不存在 = 已是满桶,什么都不做,返回 0;还回去之后满了就删键(键不存在 = 满桶),否则写回并按补满时刻设过期,返回 1。
     */
    static final String REFUND_SCRIPT = """
            local now
            if ARGV[1] == '' then
              local tm = redis.call('TIME')
              now = tonumber(tm[1]) * 1000 + math.floor(tonumber(tm[2]) / 1000)
            else
              now = tonumber(ARGV[1])
            end
            local rps, burst = tonumber(ARGV[2]), tonumber(ARGV[3]) * 1000
            local state = redis.call('HMGET', KEYS[1], 't', 'ms')
            local t, last = tonumber(state[1]), tonumber(state[2])
            if t == nil or last == nil then
              return 0
            end
            local level = math.min(burst, t + math.max(0, now - last) * rps) + 1000
            if level >= burst then
              redis.call('DEL', KEYS[1])
              return 1
            end
            redis.call('HSET', KEYS[1], 't', level, 'ms', now)
            redis.call('PEXPIRE', KEYS[1], math.ceil((burst - level) / rps))
            return 1
            """;

    /** ARGV[1] 为空串:脚本取 Redis 服务器的 TIME。 */
    private static final byte[] SERVER_TIME = new byte[0];

    /** {@link #TAKE_SCRIPT} 的返回码。 */
    private static final long TAKEN = 0;
    private static final long EMPTY = 1;

    private final RedisScriptingCommands<byte[], byte[]> redis;
    /** 两段脚本的 SHA1,本地算出(不走网络),EVALSHA 用。 */
    private final String takeSha;
    private final String refundSha;

    /**
     * @param redis 单实例或集群连接的同步接口(只用到脚本命令)。集群连接按第一个键路由 EVALSHA / EVAL。
     */
    public RedisRateLimitStore(RedisScriptingCommands<byte[], byte[]> redis) {
        this.redis = redis;
        this.takeSha = redis.digest(TAKE_SCRIPT);
        this.refundSha = redis.digest(REFUND_SCRIPT);
    }

    @Override
    public Admission admit(Bucket ip, Bucket zone) {
        return run(SERVER_TIME, ip, zone);
    }

    /** 单测用:指定当前毫秒,代替 Redis 服务器的 TIME。 */
    Admission admitAt(long nowMs, Bucket ip, Bucket zone) {
        return run(number(nowMs), ip, zone);
    }

    private Admission run(byte[] now, Bucket ip, Bucket zone) {
        if (take(now, ip)[0] == EMPTY) {
            return Admission.ipEmpty();
        }
        if (zone == null) {
            return Admission.ok();
        }
        long[] zoneTake = take(now, zone);
        if (zoneTake[0] == TAKEN) {
            return Admission.ok();
        }
        refund(now, ip);
        return Admission.zoneEmpty(zoneTake[1]);
    }

    /** 跑一次 {@link #TAKE_SCRIPT},返回 {结果码, 等待毫秒}。 */
    private long[] take(byte[] now, Bucket bucket) {
        List<Object> reply = eval(takeSha, TAKE_SCRIPT, ScriptOutputType.MULTI, bucket, now);
        if (reply == null || reply.size() != 2 || !(reply.get(0) instanceof Long kind) || !(reply.get(1) instanceof Long waitMs)
                || (kind != TAKEN && kind != EMPTY)) {
            throw new IllegalStateException("rate-limit take script returned an unknown reply: " + reply);
        }
        return new long[] {kind, waitMs};
    }

    /** 还回一个 IP 令牌;失败只打 WARN(偏严,见类注释),不抛。 */
    private void refund(byte[] now, Bucket bucket) {
        try {
            eval(refundSha, REFUND_SCRIPT, ScriptOutputType.INTEGER, bucket, now);
        } catch (RuntimeException e) {
            log.warn("rate-limit refund of one ip token failed; the ip bucket keeps one extra token spent until it refills: {}",
                    e.toString());
        }
    }

    private <T> T eval(String sha, String script, ScriptOutputType type, Bucket bucket, byte[] now) {
        byte[][] keys = {utf8(bucket.key())};
        byte[][] args = {now, number(bucket.rps()), number(bucket.burst())};
        try {
            return redis.evalsha(sha, type, keys, args);
        } catch (RedisNoScriptException e) {
            // Redis 重启、SCRIPT FLUSH、主从切换,或集群里第一次落到这个节点:脚本缓存是空的。带全文执行一次,
            // Redis 同时把它缓存下来,之后照旧走 EVALSHA。NOSCRIPT 时脚本没有执行,回退不会重复扣。
            return redis.eval(script, type, keys, args);
        }
    }

    private static byte[] utf8(String value) {
        return value.getBytes(StandardCharsets.UTF_8);
    }

    private static byte[] number(long value) {
        return Long.toString(value).getBytes(StandardCharsets.US_ASCII);
    }
}
