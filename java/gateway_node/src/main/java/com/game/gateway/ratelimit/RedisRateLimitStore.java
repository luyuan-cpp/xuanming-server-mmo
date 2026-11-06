package com.game.gateway.ratelimit;

import io.lettuce.core.RedisNoScriptException;
import io.lettuce.core.ScriptOutputType;
import io.lettuce.core.api.sync.RedisCommands;

import java.nio.charset.StandardCharsets;
import java.util.List;

/**
 * {@link RateLimitStore} 的 Redis 实现(2026-11-05,服务器缺口清单第 29 条,替代原 Bucket4j 的 CAS 后端)。
 *
 * <p><b>为什么换掉 Bucket4j。</b>Bucket4j 的 Lettuce 后端是「读状态 → 本地算 → CAS 写回,冲突就重试」。区桶
 * {@code rl:zone:<id>} 是全区请求共用的热键,副本越多 CAS 冲突越多,重试把一次桶操作拖过时限就按异常放行 ——
 * 开服洪峰正是最该限流的时候,它却最容易漏。这里改成一段 Lua:Redis 单线程执行,两个桶一次判完,没有冲突也没有重试,
 * 每次检查固定一个往返。
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
 * <p><b>时钟。</b>取 Redis 的 TIME,全部副本一个时钟。Redis 所在机器的时钟往回跳时按经过 0 毫秒算、不补令牌,
 * 最多卡住回跳的那么久;下一次放行把 ms 写成新时刻,之后恢复正常。
 *
 * <p><b>部署前提。</b>两个键在同一段 Lua 里,要求在同一个 Redis 实例上。现状是 gateway 只连本 zone 组的 redis-queue
 * 单实例(见 {@code docs/design/shared-redis-split-by-purpose.md});将来改 Redis Cluster,两键不同槽,要拆成两段单键脚本
 * (IP 桶扣过再判区桶,区桶空时另退还 IP 令牌)。
 *
 * <p><b>超时与重发。</b>命令超时由专用 Lettuce 连接约束({@link RateLimitConfig#REDIS_COMMAND_TIMEOUT})。Lettuce
 * 不重发超时的命令;超时的那一次若其实已在 Redis 上执行,就多扣了一个令牌(偏严,可接受)。
 *
 * <p>线程安全(Lettuce 同步接口可多线程共用一条连接);阻塞,只在请求线程上调用。
 */
public final class RedisRateLimitStore implements RateLimitStore {

    /**
     * KEYS:ip[, zone];ARGV:now_ms(空串 = 取 Redis 服务器的 TIME), ip_rps, ip_burst[, zone_rps, zone_burst]。
     * 返回 {0} 放行(两个桶各扣一个令牌) / {1} IP 桶空 / {2, wait_ms} 区桶空(补到一个令牌还要等的毫秒,向上取整)。
     * 语义见类注释,脚本正文不写注释。
     */
    static final String ADMIT_SCRIPT = """
            local now
            if ARGV[1] == '' then
              local tm = redis.call('TIME')
              now = tonumber(tm[1]) * 1000 + math.floor(tonumber(tm[2]) / 1000)
            else
              now = tonumber(ARGV[1])
            end
            local function level(key, rps, burst)
              local state = redis.call('HMGET', key, 't', 'ms')
              local t, last = tonumber(state[1]), tonumber(state[2])
              if t == nil or last == nil then
                return burst
              end
              return math.min(burst, t + math.max(0, now - last) * rps)
            end
            local function take(key, current, rps, burst)
              local left = current - 1000
              redis.call('HSET', key, 't', left, 'ms', now)
              redis.call('PEXPIRE', key, math.ceil((burst - left) / rps))
            end
            local ipRps, ipBurst = tonumber(ARGV[2]), tonumber(ARGV[3]) * 1000
            local ip = level(KEYS[1], ipRps, ipBurst)
            if ip < 1000 then
              return {1}
            end
            if #KEYS == 1 then
              take(KEYS[1], ip, ipRps, ipBurst)
              return {0}
            end
            local zoneRps, zoneBurst = tonumber(ARGV[4]), tonumber(ARGV[5]) * 1000
            local zone = level(KEYS[2], zoneRps, zoneBurst)
            if zone < 1000 then
              return {2, math.ceil((1000 - zone) / zoneRps)}
            end
            take(KEYS[1], ip, ipRps, ipBurst)
            take(KEYS[2], zone, zoneRps, zoneBurst)
            return {0}
            """;

    /** ARGV[1] 为空串:脚本取 Redis 服务器的 TIME。 */
    private static final byte[] SERVER_TIME = new byte[0];

    private final RedisCommands<byte[], byte[]> redis;
    /** 脚本的 SHA1,本地算出(不走网络),EVALSHA 用。 */
    private final String admitSha;

    public RedisRateLimitStore(RedisCommands<byte[], byte[]> redis) {
        this.redis = redis;
        this.admitSha = redis.digest(ADMIT_SCRIPT);
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
        byte[][] keys;
        byte[][] args;
        if (zone == null) {
            keys = new byte[][] {utf8(ip.key())};
            args = new byte[][] {now, number(ip.rps()), number(ip.burst())};
        } else {
            keys = new byte[][] {utf8(ip.key()), utf8(zone.key())};
            args = new byte[][] {now, number(ip.rps()), number(ip.burst()), number(zone.rps()), number(zone.burst())};
        }
        List<Object> reply = eval(keys, args);
        long kind = (Long) reply.get(0);
        if (kind == 0) {
            return Admission.ok();
        }
        if (kind == 1) {
            return Admission.ipEmpty();
        }
        if (kind == 2) {
            return Admission.zoneEmpty((Long) reply.get(1));
        }
        throw new IllegalStateException("限流脚本返回了未知结果: " + reply);
    }

    private List<Object> eval(byte[][] keys, byte[][] args) {
        try {
            return redis.evalsha(admitSha, ScriptOutputType.MULTI, keys, args);
        } catch (RedisNoScriptException e) {
            // Redis 重启、SCRIPT FLUSH 或主从切换后脚本缓存是空的:带全文执行一次,Redis 同时把它缓存下来,之后照旧走 EVALSHA。
            return redis.eval(ADMIT_SCRIPT, ScriptOutputType.MULTI, keys, args);
        }
    }

    private static byte[] utf8(String value) {
        return value.getBytes(StandardCharsets.UTF_8);
    }

    private static byte[] number(long value) {
        return Long.toString(value).getBytes(StandardCharsets.US_ASCII);
    }
}
