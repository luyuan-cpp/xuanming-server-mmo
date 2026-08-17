package com.game.gateway.controller;

import com.game.gateway.dto.LoginRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.LoginService;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * HTTP entry point that finishes the heavy login work (OAuth verification,
 * player loading, token issuance) <b>before</b> the client touches a Gate.
 *
 * <p>Two-step flow:
 * <ol>
 *   <li>{@code POST /api/login} — this controller. Verifies third-party token
 *       (or password), receives access/refresh tokens, surfaces queue/throttle
 *       decisions to the client.</li>
 *   <li>{@code POST /api/assign-gate} — already exists. Picks a gate and
 *       signs the HMAC handshake token.</li>
 * </ol>
 */
@RestController
@RequestMapping("/api")
public class LoginController {

    private static final Logger log = LoggerFactory.getLogger(LoginController.class);

    private final LoginService loginService;
    private final AssignGateRateLimiter limiter;
    private final ClientIpResolver ipResolver;

    public LoginController(LoginService loginService, AssignGateRateLimiter limiter,
                           ClientIpResolver ipResolver) {
        this.loginService = loginService;
        this.limiter = limiter;
        this.ipResolver = ipResolver;
    }

    @PostMapping("/login")
    public LoginResponse login(@RequestBody LoginRequest req, HttpServletRequest http) {
        String ip = ipResolver.resolve(http);
        String account = effectiveAccount(req);

        // cooldownScope="login":与 /api/assign-gate 的账号冷却隔离,否则
        // 「login 成功→立刻 assign-gate」的正常顺序会撞 ACCOUNT_COOLDOWN。
        RateLimitDecision decision = limiter.check(req.getZoneId(), ip, account, "login");
        if (decision.isQueue()) {
            return LoginResponse.queueing(decision.getRetryAfterMs(), decision.getQueuePosEstimate());
        }
        if (decision.isDeny()) {
            return LoginResponse.error(LoginResponse.CODE_RATE_LIMITED, decision.getReason());
        }

        return loginService.login(req);
    }

    /** For third-party auth the proto's {@code account} field is empty; use auth_token as the cooldown key. */
    private static String effectiveAccount(LoginRequest req) {
        if (req.getAccount() != null && !req.getAccount().isBlank()) {
            return req.getAccount();
        }
        if (req.getAuthToken() != null && !req.getAuthToken().isBlank()) {
            // Hash to bound key length and avoid logging raw tokens.
            return "tok:" + Integer.toHexString(req.getAuthToken().hashCode());
        }
        return null;
    }
}
