package com.game.gateway.controller;

import com.game.gateway.dto.RefreshTokenRequest;
import com.game.gateway.dto.RefreshTokenResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.RefreshTokenService;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * Dedicated HTTP endpoint for refresh-token rotation.
 *
 * <p>Refresh-token rotation does not need session affinity or a TCP
 * connection to the gate — it's a stateless Redis lookup in go-zero login.
 * Keeping it on HTTP moves the rotation path out of the game TCP channel so
 * gate stays focused on connected-player traffic.
 *
 * <p>仅挂共享 IP 令牌桶({@link AssignGateRateLimiter#checkIpOnly}):跳过开服波次、区桶与账号冷却,
 * 不拦截正常在游戏内或排队中续期的客户端,同时挡住单 IP 刷接口(见 open-server-rate-limit-design.md §3)。
 */
@RestController
@RequestMapping("/api")
public class RefreshTokenController {

    private final RefreshTokenService service;
    private final AssignGateRateLimiter limiter;
    private final ClientIpResolver ipResolver;

    public RefreshTokenController(RefreshTokenService service, AssignGateRateLimiter limiter,
                                  ClientIpResolver ipResolver) {
        this.service = service;
        this.limiter = limiter;
        this.ipResolver = ipResolver;
    }

    @PostMapping("/refresh-token")
    public RefreshTokenResponse refresh(@RequestBody RefreshTokenRequest req, HttpServletRequest http) {
        RateLimitDecision decision = limiter.checkIpOnly(ipResolver.resolve(http));
        if (decision.isDeny()) {
            return RefreshTokenResponse.error(RefreshTokenResponse.CODE_RATE_LIMITED, decision.getReason());
        }
        return service.refresh(req);
    }
}
