package com.game.gateway.controller;

import com.game.gateway.dto.AccountDeletionRequest;
import com.game.gateway.dto.AccountDeletionResponse;
import com.game.gateway.dto.CancelAccountDeletionRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.AccountDeletionService;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * 账号注销的申请与撤销(docs/design/account-deletion.md §4.2、§4.4、§5.2,客户端可见契约,两版同批)。
 *
 * <p>先做边界校验(不合法回 401 + 稳定原因串,不碰限流器也不调 login),再过与 /api/login 同一个限流器、同一个冷却 scope
 * {@code "login"}(设计:限流沿用登录那一桶),最后交给 {@link AccountDeletionService}。限流的排队判定也回 429:注销没有排队语义。
 * 冷却键:口令账号用账号本身(与 /api/login 相同),第三方凭据与 access token 只用 SHA-256 摘要
 * ({@link AssignGateController#cooldownKey}),原文不进限流键,也不进日志。
 */
@RestController
@RequestMapping("/api")
public class AccountDeletionController {

    /**
     * 字段长度上限,与 login 自己的上限一致(go/login/internal/logic/pkg/auth:账号 191 个码点、口令 1024 字节、
     * 身份令牌 16 KiB)。口令与令牌按 UTF-16 长度比,不会比 login 严;只为不把超长串放进内存里的冷却键和 gRPC。
     */
    static final int MAX_ACCOUNT_CODE_POINTS = 191;
    static final int MAX_PASSWORD_CHARS = 1024;
    static final int MAX_TOKEN_CHARS = 16 << 10;

    /** 与 /api/login 共用冷却窗口:交替调两个接口不能各得一份口令尝试的窗口。 */
    private static final String COOLDOWN_SCOPE = "login";

    private final AccountDeletionService service;
    private final AssignGateRateLimiter limiter;
    private final ClientIpResolver ipResolver;

    public AccountDeletionController(AccountDeletionService service, AssignGateRateLimiter limiter,
                                     ClientIpResolver ipResolver) {
        this.service = service;
        this.limiter = limiter;
        this.ipResolver = ipResolver;
    }

    @PostMapping("/account/delete")
    public AccountDeletionResponse requestDeletion(@RequestBody AccountDeletionRequest req, HttpServletRequest http) {
        String invalid = invalidReason(req);
        if (invalid != null) {
            return AccountDeletionResponse.error(LoginResponse.CODE_AUTH_REJECTED, invalid);
        }
        String account = req.getAccount();
        String cooldownKey = account != null && !account.isBlank()
                ? account
                : AssignGateController.cooldownKey(req.getAuthToken());
        AccountDeletionResponse limited = rateLimited(req.getZoneId(), http, cooldownKey);
        return limited != null ? limited : service.requestDeletion(req);
    }

    @PostMapping("/account/delete/cancel")
    public AccountDeletionResponse cancelDeletion(@RequestBody CancelAccountDeletionRequest req, HttpServletRequest http) {
        String invalid = invalidReason(req);
        if (invalid != null) {
            return AccountDeletionResponse.error(LoginResponse.CODE_AUTH_REJECTED, invalid);
        }
        AccountDeletionResponse limited =
                rateLimited(req.getZoneId(), http, AssignGateController.cooldownKey(req.getAccessToken()));
        return limited != null ? limited : service.cancelDeletion(req);
    }

    /** null = 放行;否则是 429 应答(message 是限流器的原因串:QUEUEING / IP_RATE_LIMIT / ACCOUNT_COOLDOWN)。 */
    private AccountDeletionResponse rateLimited(long zoneId, HttpServletRequest http, String cooldownKey) {
        RateLimitDecision decision = limiter.check(zoneId, ipResolver.resolve(http), cooldownKey, COOLDOWN_SCOPE);
        return decision.isPass() ? null : AccountDeletionResponse.error(LoginResponse.CODE_RATE_LIMITED, decision.getReason());
    }

    /** 合法返回 null。zone_id 必须能按区选路(正数、在 int 范围内:LoginRpcClient 按 int 选路)。 */
    static String invalidReason(AccountDeletionRequest req) {
        if (!routableZone(req.getZoneId())) {
            return "invalid_zone_id";
        }
        String account = req.getAccount();
        if ((account != null && account.codePointCount(0, account.length()) > MAX_ACCOUNT_CODE_POINTS)
                || longer(req.getPassword(), MAX_PASSWORD_CHARS)
                || longer(req.getAuthType(), MAX_TOKEN_CHARS)
                || longer(req.getAuthToken(), MAX_TOKEN_CHARS)
                || longer(req.getAuthorizationCode(), MAX_TOKEN_CHARS)) {
            return "field_too_long";
        }
        return null;
    }

    static String invalidReason(CancelAccountDeletionRequest req) {
        if (!routableZone(req.getZoneId())) {
            return "invalid_zone_id";
        }
        if (req.getAccessToken() == null || req.getAccessToken().isBlank()) {
            return "empty_access_token";
        }
        return longer(req.getAccessToken(), MAX_TOKEN_CHARS) ? "field_too_long" : null;
    }

    private static boolean routableZone(long zoneId) {
        return zoneId > 0 && zoneId <= Integer.MAX_VALUE;
    }

    private static boolean longer(String s, int max) {
        return s != null && s.length() > max;
    }
}
