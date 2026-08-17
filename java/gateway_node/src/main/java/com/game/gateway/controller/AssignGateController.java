package com.game.gateway.controller;

import com.game.gateway.dto.AssignGateRequest;
import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.AssignGateService;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.web.bind.annotation.*;

/**
 * Two-layer admission control on {@code POST /api/assign-gate}:
 *
 * <ol>
 *   <li><b>{@link AssignGateRateLimiter} (Bucket4j):</b> first-line filter
 *       against retry storms and zone-wave throttling. Returns code=100
 *       with {@code queueSource="ratelimit"} when the caller should retry
 *       later — no real position information is available because Bucket4j
 *       is a token bucket, not an ordered queue.</li>
 *   <li><b>go-zero login {@link AssignGateService}:</b> authoritative
 *       admission. Either signs a gate token immediately (capacity available)
 *       or enqueues the caller in the Redis ZSET-backed login queue. When
 *       enqueued, returns code=100 with {@code queueSource="login"} and
 *       a real {@code queueRank}/{@code queueTotal} for the UI.</li>
 * </ol>
 *
 * <p>The two layers compose: Bucket4j keeps the gRPC fan-out cheap by
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
        // Re-entry path (queue_token present) skips the limiter — the caller
        // is already in the queue and is just polling. Throttling them would
        // surface as a UX bug ("rank stops decreasing"). The queue itself
        // is the throttle for repeated re-entries.
        if (req.getQueueToken() != null && !req.getQueueToken().isBlank()) {
            return assignGateService.assignGate(req);
        }

        RateLimitDecision decision = limiter.check(req.getZoneId(), ip, req.getAccount());
        if (decision.isQueue()) {
            return AssignGateResponse.queueing(decision.getRetryAfterMs(), decision.getQueuePosEstimate());
        }
        if (decision.isDeny()) {
            return AssignGateResponse.rateLimited(decision.getReason());
        }
        return assignGateService.assignGate(req);
    }
}
