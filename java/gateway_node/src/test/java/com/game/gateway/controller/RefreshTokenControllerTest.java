package com.game.gateway.controller;

import com.game.gateway.dto.RefreshTokenRequest;
import com.game.gateway.dto.RefreshTokenResponse;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.RefreshTokenService;
import jakarta.servlet.http.HttpServletRequest;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link RefreshTokenController} 的限流契约测试:
 * {@code /api/refresh-token} 只过共享 IP 令牌桶({@link AssignGateRateLimiter#checkIpOnly}),
 * IP 桶空时回 {@code code=429} 且不打下游 {@link RefreshTokenService},放行时原样转发。
 */
class RefreshTokenControllerTest {

    private static final String CLIENT_IP = "198.51.100.7";

    private RefreshTokenService service;
    private AssignGateRateLimiter limiter;
    private HttpServletRequest http;
    private RefreshTokenController controller;

    @BeforeEach
    void setUp() {
        service = mock(RefreshTokenService.class);
        limiter = mock(AssignGateRateLimiter.class);
        ClientIpResolver ipResolver = mock(ClientIpResolver.class);
        http = mock(HttpServletRequest.class);
        when(ipResolver.resolve(http)).thenReturn(CLIENT_IP);
        controller = new RefreshTokenController(service, limiter, ipResolver);
    }

    @Test
    void ipRateLimited_returns429WithoutCallingService() {
        when(limiter.checkIpOnly(CLIENT_IP)).thenReturn(RateLimitDecision.deny("IP_RATE_LIMIT"));

        RefreshTokenRequest req = new RefreshTokenRequest();
        req.setRefreshToken("rt-1");
        RefreshTokenResponse resp = controller.refresh(req, http);

        verify(service, never()).refresh(any());
        assertEquals(RefreshTokenResponse.CODE_RATE_LIMITED, resp.getCode());
        assertEquals("IP_RATE_LIMIT", resp.getMessage());
    }

    @Test
    void allowed_forwardsRequestToService() {
        when(limiter.checkIpOnly(CLIENT_IP)).thenReturn(RateLimitDecision.pass());
        RefreshTokenRequest req = new RefreshTokenRequest();
        req.setRefreshToken("rt-2");
        RefreshTokenResponse expected = RefreshTokenResponse.ok("at-2", "rt-3", 100L, 200L);
        when(service.refresh(req)).thenReturn(expected);

        RefreshTokenResponse actual = controller.refresh(req, http);

        verify(limiter).checkIpOnly(CLIENT_IP);
        verify(service).refresh(req);
        assertSame(expected, actual);
    }
}
