package com.game.gateway.ratelimit;

/**
 * 限流的共享存储:全部 gateway 副本共用的令牌桶(生产实现 {@link RedisRateLimitStore},单测换成脚本化的假实现)。
 *
 * <p>时间由存储自己取(Redis 实现取服务器的 TIME):桶由所有副本共享,各副本时钟有偏差时,时钟快的副本会把
 * 「上次补充时刻」写到将来,别的副本就补不上令牌。
 *
 * <p>线程安全;Redis 实现是阻塞调用,只在请求线程上用。出错一律抛 {@link RuntimeException},由
 * {@link AssignGateRateLimiter} 按 fail-open 处理。
 */
public interface RateLimitStore {

    /**
     * 先判 IP 桶、再判区桶,一次原子地决定:
     * <ul>
     *   <li>IP 桶不足一个令牌 → {@link Admission.Kind#IP_EMPTY},两个桶都不扣;</li>
     *   <li>区桶不足一个令牌 → {@link Admission.Kind#ZONE_EMPTY},两个桶都不扣(排队中的重试不吃 IP 桶);</li>
     *   <li>都够 → {@link Admission.Kind#OK},两个桶各扣一个令牌。</li>
     * </ul>
     *
     * @param zone 区桶;null = 只判 IP 桶(zone_id = 0 的「任选一区」请求)
     */
    Admission admit(Bucket ip, Bucket zone);

    /**
     * 一个令牌桶:容量 {@code burst} 个令牌,每秒均匀补 {@code rps} 个(与原 Bucket4j 的贪心补充同义)。
     * 两个值都必须为正,由 {@link RateLimitConfig} 在启动时校验。
     */
    record Bucket(String key, long rps, long burst) {
    }

    /**
     * @param waitMs 只对 {@link Kind#ZONE_EMPTY} 有意义:区桶补到一个令牌还要等的毫秒(向上取整,至少 1),
     *               即客户端收到的 retry_after_ms
     */
    record Admission(Kind kind, long waitMs) {

        public enum Kind { OK, IP_EMPTY, ZONE_EMPTY }

        static Admission ok() {
            return new Admission(Kind.OK, 0);
        }

        static Admission ipEmpty() {
            return new Admission(Kind.IP_EMPTY, 0);
        }

        static Admission zoneEmpty(long waitMs) {
            return new Admission(Kind.ZONE_EMPTY, waitMs);
        }
    }
}
