package com.game.gateway.controller;

import com.game.gateway.dto.AssignGateRequest;
import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.AssignGateService;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.web.bind.annotation.*;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;

/**
 * Two-layer admission control on {@code POST /api/assign-gate}:
 *
 * <ol>
 *   <li><b>{@link AssignGateRateLimiter} (Redis token buckets):</b> first-line filter
 *       against retry storms and zone-wave throttling. Returns code=100
 *       with {@code queueSource="ratelimit"} when the caller should retry
 *       later — no real position information is available because the
 *       limiter is a token bucket, not an ordered queue.</li>
 *   <li><b>go-zero login {@link AssignGateService}:</b> authoritative
 *       admission. Either signs a gate token immediately (capacity available)
 *       or enqueues the caller in the Redis ZSET-backed login queue. When
 *       enqueued, returns code=100 with {@code queueSource="login"} and
 *       a real {@code queueRank}/{@code queueTotal} for the UI.</li>
 * </ol>
 *
 * <p>The two layers compose: the limiter keeps the gRPC fan-out cheap by
 * rejecting obvious abuse before it reaches login; the queue handles the
 * "everyone is legitimate but too many of them showed up at once" case
 * (open-server day, world-boss reset, etc.).
 */
@RestController
@RequestMapping("/api")
public class AssignGateController {

    private final AssignGateService assignGateService;
    private final AssignGateRateLimiter limiter;
    private final ClientIpResolver ipResolver;

    public AssignGateController(AssignGateService assignGateService, AssignGateRateLimiter limiter,
                                ClientIpResolver ipResolver) {
        this.assignGateService = assignGateService;
        this.limiter = limiter;
        this.ipResolver = ipResolver;
    }

    @PostMapping("/assign-gate")
    public AssignGateResponse assignGate(@RequestBody AssignGateRequest req, HttpServletRequest http) {
        String ip = ipResolver.resolve(http);
        // 带 queue_token 的重入照样过 zone 桶与 IP 桶,只跳过账号冷却。gateway 验不了 token(密钥在 login),
        // 也不知道 login 排队开没开 —— 排队关着时 login 对带 token 的请求直接走快速通道签票,无条件放行 =
        // 任意伪造一个字段就绕过整个限流。持有 token 的客户端按契约轮询 /api/queue-status(不限流);带 token
        // 重调 assign-gate 只作为 queue-status 本身失败(网络错误 / HTTP 非 200 / code=500;503 zone_* 与 404 是终态,
        // 不重入)后的恢复手段,偶尔多扣一个令牌可以接受。
        // 重入在这里被挡住(code=100 ratelimit 或 429)时客户端回到 queue-status 轮询,不退避重调本接口,
        // 契约见 AssignGateRequest#queueToken。冷却要跳过:恢复重入常在 5s 窗口内,会被自己挡住。
        boolean reentry = req.getQueueToken() != null && !req.getQueueToken().isBlank();
        RateLimitDecision decision = limiter.check(req.getZoneId(), ip, reentry ? null : cooldownKey(req.getAccessToken()));
        if (decision.isQueue()) {
            return AssignGateResponse.queueing(decision.getRetryAfterMs(), decision.getQueuePosEstimate());
        }
        if (decision.isDeny()) {
            return AssignGateResponse.rateLimited(decision.getReason());
        }
        return assignGateService.assignGate(req);
    }

    /**
     * 账号冷却的键。请求里没有可信的账号(账号只有 login 验过令牌之后才知道),所以按 access token 的 SHA-256 摘要分桶:
     * "tok:" + 前 16 字节的十六进制。同一次登录签出的令牌共用一个冷却。不带令牌(匿名分配)返回 null,限流器对 null
     * 跳过冷却,与以前不带 account 的请求相同。令牌原文不进 Redis 键,也不进日志。
     */
    static String cooldownKey(String accessToken) {
        if (accessToken == null || accessToken.isBlank()) {
            return null;
        }
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256").digest(accessToken.getBytes(StandardCharsets.UTF_8));
            return "tok:" + HexFormat.of().formatHex(digest, 0, 16);
        } catch (NoSuchAlgorithmException e) {
            // 每个 JDK 都必须提供 SHA-256(Java SE 规范),走到这里是运行时损坏。
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }
}
