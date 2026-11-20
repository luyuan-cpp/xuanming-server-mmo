package com.game.gateway.ratelimit;

import org.springframework.boot.context.properties.ConfigurationProperties;

import java.util.List;
import java.util.Map;

/**
 * Rate-limit & wave-schedule config for /api/login and /api/assign-gate.
 *
 * Design: docs/design/open-server-rate-limit-design.md
 *
 * Default is <b>disabled</b> — limiter acts as a pass-through until operations
 * explicitly turn it on per-zone.
 */
@ConfigurationProperties(prefix = "gate.rate-limit")
public class RateLimitProperties {

    /** Master switch. false -> limiter is a no-op (fail-open). */
    private boolean enabled = false;

    /** Default tokens/sec refill for a zone bucket when no override is set. */
    private long zoneDefaultRps = 500;

    /** Default burst capacity for a zone bucket. */
    private long zoneDefaultBurst = 1000;

    /** Per-zone overrides, e.g. {"1": {rps:2000, burst:5000}}. */
    private Map<Long, ZoneLimit> zoneOverrides = Map.of();

    /** Per-IP requests per second. */
    private long ipRps = 2;

    /** Per-IP burst capacity. */
    private long ipBurst = 10;

    /**
     * 账号冷却窗口(ms):同一端点上同一(账号, 客户端 IP)两次请求的最小间隔。
     * /api/login 与 /api/assign-gate 按 scope 各自计时;带 queue_token 的 assign-gate 重入不参与冷却
     * (G4b-J0);键含客户端 IP 的原因见 {@link AssignGateRateLimiter} 类注释(G4b-J2)。
     * 冷却在进程内,每个 gateway 副本各一份。
     */
    private long accountCooldownMs = 5000;

    /**
     * 排队提示(波次未开、区桶空)里 retry_after_ms 的下限(ms),必须为正。区桶补到一个令牌往往只要 1~2ms
     * (2000 rps 时 0.5ms),原样下发等于让排队的人每个往返重试一次,全都打在同一个 {@code rl:zone:<id>} 键上;
     * 有了下限,一个守规矩的客户端排队时每秒最多 1 / 下限 次。与登录排队的基础轮询间隔(2 秒)一致。
     */
    private long queueRetryMinMs = 2000;

    /**
     * 排队提示在下限之上再加的均匀随机量的上界(ms,取 [0, 这个值)),0 = 不加。同一时刻被拒的一批人(波次开放那一刻、
     * 区桶刚被抽空时)会在同一时刻回来,随机量把他们摊到这么宽的时间窗里。
     */
    private long queueRetryJitterMs = 2000;

    /**
     * 区桶空(排队)时 IP 桶扣多少,以千分之一令牌为单位(100 = 0.1 个),取 0~1000。区桶空时以前把 IP 令牌全额退还,
     * 排队中的重试不受 IP 桶约束,不守 retry_after 的单个 IP 可以无限速地打区桶热键;扣一小份之后,一个 IP 排队时
     * 最多每秒 ip-rps × 1000 / 这个值 次(缺省 5 × 10 = 50 次),守规矩的客户端每 2~4 秒一次,同一出口 IP 后面
     * 约 150 人同时排队才会碰到 IP 桶。0 = 回到全额退还;1000 = 与放行一样扣一个。
     */
    private long queueRetryIpCostMilli = 100;

    /** Hard queue timeout (ms). After this the client gets QUEUE_TIMEOUT. */
    private long queueTimeoutMs = 300_000;

    /**
     * 可信反向代理的 CIDR 列表(如 10.0.0.0/8)。**只有**当 socket 对端落在这些网段内,
     * 才解析 X-Forwarded-For;否则一律用 socket 对端地址。
     *
     * <p>默认空 = 完全不信任 XFF。这是 fail-closed:XFF 是客户端可任意书写的头,
     * 无条件采信会让 IP 维度限流被随机头绕过(见 {@link ClientIpResolver} 类注释)。
     * 部署在 ingress / LB 之后时**必须**配置,否则所有请求共用 LB 那一个 IP 桶。
     */
    private List<String> trustedProxies = List.of();

    /**
     * Open-server wave schedule. When present, zones outside the current wave
     * are forced to queue regardless of remaining token count.
     *
     * Example:
     *   schedule:
     *     - { offsetSec: 0,   allowZones: [1, 2] }
     *     - { offsetSec: 30,  allowZones: [3, 4] }
     *     - { offsetSec: 120, allowZones: ALL }   # -1 means "all zones"
     */
    private WaveSchedule wave = new WaveSchedule();

    public static class ZoneLimit {
        private long rps;
        private long burst;
        public long getRps() { return rps; }
        public void setRps(long rps) { this.rps = rps; }
        public long getBurst() { return burst; }
        public void setBurst(long burst) { this.burst = burst; }
    }

    public static class WaveSchedule {
        private boolean enabled = false;
        /** Epoch-seconds of the wave baseline. 0 = use application boot time. */
        private long startEpochSec = 0;
        private List<WaveStep> schedule = List.of();

        public boolean isEnabled() { return enabled; }
        public void setEnabled(boolean enabled) { this.enabled = enabled; }
        public long getStartEpochSec() { return startEpochSec; }
        public void setStartEpochSec(long startEpochSec) { this.startEpochSec = startEpochSec; }
        public List<WaveStep> getSchedule() { return schedule; }
        public void setSchedule(List<WaveStep> schedule) { this.schedule = schedule; }
    }

    public static class WaveStep {
        private long offsetSec;
        /** List of zone ids. Use the single value -1 to mean "all zones". */
        private List<Long> allowZones = List.of();

        public long getOffsetSec() { return offsetSec; }
        public void setOffsetSec(long offsetSec) { this.offsetSec = offsetSec; }
        public List<Long> getAllowZones() { return allowZones; }
        public void setAllowZones(List<Long> allowZones) { this.allowZones = allowZones; }
    }

    // getters / setters
    public boolean isEnabled() { return enabled; }
    public void setEnabled(boolean enabled) { this.enabled = enabled; }
    public long getZoneDefaultRps() { return zoneDefaultRps; }
    public void setZoneDefaultRps(long zoneDefaultRps) { this.zoneDefaultRps = zoneDefaultRps; }
    public long getZoneDefaultBurst() { return zoneDefaultBurst; }
    public void setZoneDefaultBurst(long zoneDefaultBurst) { this.zoneDefaultBurst = zoneDefaultBurst; }
    public Map<Long, ZoneLimit> getZoneOverrides() { return zoneOverrides; }
    public void setZoneOverrides(Map<Long, ZoneLimit> zoneOverrides) { this.zoneOverrides = zoneOverrides; }
    public long getIpRps() { return ipRps; }
    public void setIpRps(long ipRps) { this.ipRps = ipRps; }
    public long getIpBurst() { return ipBurst; }
    public void setIpBurst(long ipBurst) { this.ipBurst = ipBurst; }
    public long getAccountCooldownMs() { return accountCooldownMs; }
    public void setAccountCooldownMs(long accountCooldownMs) { this.accountCooldownMs = accountCooldownMs; }
    public long getQueueRetryMinMs() { return queueRetryMinMs; }
    public void setQueueRetryMinMs(long queueRetryMinMs) { this.queueRetryMinMs = queueRetryMinMs; }
    public long getQueueRetryJitterMs() { return queueRetryJitterMs; }
    public void setQueueRetryJitterMs(long queueRetryJitterMs) { this.queueRetryJitterMs = queueRetryJitterMs; }
    public long getQueueRetryIpCostMilli() { return queueRetryIpCostMilli; }
    public void setQueueRetryIpCostMilli(long queueRetryIpCostMilli) { this.queueRetryIpCostMilli = queueRetryIpCostMilli; }
    public long getQueueTimeoutMs() { return queueTimeoutMs; }
    public void setQueueTimeoutMs(long queueTimeoutMs) { this.queueTimeoutMs = queueTimeoutMs; }

    public List<String> getTrustedProxies() { return trustedProxies; }
    public void setTrustedProxies(List<String> trustedProxies) {
        this.trustedProxies = trustedProxies == null ? List.of() : trustedProxies;
    }
    public WaveSchedule getWave() { return wave; }
    public void setWave(WaveSchedule wave) { this.wave = wave; }
}
