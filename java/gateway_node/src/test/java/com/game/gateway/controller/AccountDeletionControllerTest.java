package com.game.gateway.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.game.gateway.dto.AccountDeletionRequest;
import com.game.gateway.dto.AccountDeletionResponse;
import com.game.gateway.dto.CancelAccountDeletionRequest;
import com.game.gateway.ratelimit.AssignGateRateLimiter;
import com.game.gateway.ratelimit.ClientIpResolver;
import com.game.gateway.ratelimit.RateLimitDecision;
import com.game.gateway.service.AccountDeletionService;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.http.MediaType;
import org.springframework.http.converter.json.Jackson2ObjectMapperBuilder;
import org.springframework.http.converter.json.MappingJackson2HttpMessageConverter;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.verifyNoInteractions;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/**
 * {@code POST /api/account/delete} 与 {@code /api/account/delete/cancel} 的 HTTP 契约(docs/design/account-deletion.md §5.2):
 * snake_case 的请求绑定与应答字段名、边界校验、限流(与 /api/login 同一个限流器与冷却 scope,令牌原文不进限流键)。
 * login 应答怎么映射由 {@code AccountDeletionServiceTest} 验。MockMvc standalone,Jackson 与线上同一种配置(SNAKE_CASE)。
 */
class AccountDeletionControllerTest {

    private static final long ZONE = 3L;
    private static final String CLIENT_IP = "203.0.113.9";
    private static final String AUTH_TOKEN = "SYNTHETIC_IDENTITY_TOKEN";
    private static final String ACCESS_TOKEN = "SYNTHETIC_ACCESS_TOKEN";

    private AccountDeletionService service;
    private AssignGateRateLimiter limiter;
    private MockMvc mvc;

    @BeforeEach
    void setUp() {
        service = mock(AccountDeletionService.class);
        limiter = mock(AssignGateRateLimiter.class);
        ClientIpResolver ipResolver = mock(ClientIpResolver.class);
        when(ipResolver.resolve(any())).thenReturn(CLIENT_IP);
        when(limiter.check(anyLong(), any(), any(), any())).thenReturn(RateLimitDecision.pass());
        ObjectMapper json = Jackson2ObjectMapperBuilder.json()
                .propertyNamingStrategy(PropertyNamingStrategies.SNAKE_CASE).build();
        mvc = MockMvcBuilders.standaloneSetup(new AccountDeletionController(service, limiter, ipResolver))
                .setMessageConverters(new MappingJackson2HttpMessageConverter(json))
                .build();
    }

    @Test
    void delete_bindsSnakeCaseBodyAndRendersContractFields() throws Exception {
        AccountDeletionResponse.Blocker blocker = new AccountDeletionResponse.Blocker();
        blocker.setPlayerId(101L);
        blocker.setReason("guild_leader");
        when(service.requestDeletion(any()))
                .thenReturn(AccountDeletionResponse.answered(401, "upstream_err=2039", 2039, 0, 0L, List.of(blocker)));

        mvc.perform(post("/api/account/delete").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3,\"auth_type\":\"apple\",\"auth_token\":\"" + AUTH_TOKEN
                                + "\",\"authorization_code\":\"apple-code\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(401))
                .andExpect(jsonPath("$.message").value("upstream_err=2039"))
                .andExpect(jsonPath("$.tip").value(2039))
                .andExpect(jsonPath("$.state").value(0))
                .andExpect(jsonPath("$.execute_after").value(0))
                .andExpect(jsonPath("$.blockers[0].player_id").value(101))
                .andExpect(jsonPath("$.blockers[0].reason").value("guild_leader"))
                .andExpect(jsonPath("$.executeAfter").doesNotExist());

        ArgumentCaptor<AccountDeletionRequest> bound = ArgumentCaptor.forClass(AccountDeletionRequest.class);
        verify(service).requestDeletion(bound.capture());
        assertEquals(ZONE, bound.getValue().getZoneId());
        assertEquals("apple", bound.getValue().getAuthType());
        assertEquals(AUTH_TOKEN, bound.getValue().getAuthToken());
        assertEquals("apple-code", bound.getValue().getAuthorizationCode());
        assertNull(bound.getValue().getAccount());

        // 第三方凭据:冷却键是令牌的摘要,不是原文;scope 与 /api/login 相同。
        String key = AssignGateController.cooldownKey(AUTH_TOKEN);
        verify(limiter).check(eq(ZONE), eq(CLIENT_IP), eq(key), eq("login"));
        assertFalse(key.contains(AUTH_TOKEN));
    }

    @Test
    void delete_passwordAccountAccepted_usesAccountAsLoginCooldownKey() throws Exception {
        when(service.requestDeletion(any()))
                .thenReturn(AccountDeletionResponse.answered(0, null, 0, 1, 1_800_000_000L, List.of()));

        mvc.perform(post("/api/account/delete").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3,\"account\":\"alice\",\"password\":\"pw\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(0))
                .andExpect(jsonPath("$.tip").value(0))
                .andExpect(jsonPath("$.state").value(1))
                .andExpect(jsonPath("$.execute_after").value(1_800_000_000L))
                .andExpect(jsonPath("$.message").doesNotExist())
                .andExpect(jsonPath("$.blockers").doesNotExist());

        // 与 /api/login 同一个冷却键(账号本身)与 scope:交替调两个接口不能各得一份窗口。口令不进限流器。
        verify(limiter).check(eq(ZONE), eq(CLIENT_IP), eq("alice"), eq("login"));
    }

    @Test
    void cancel_bindsZoneAndAccessToken_andUsesTokenDigestAsCooldownKey() throws Exception {
        when(service.cancelDeletion(any()))
                .thenReturn(AccountDeletionResponse.answered(0, null, 0, 0, 0L, List.of()));

        mvc.perform(post("/api/account/delete/cancel").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3,\"access_token\":\"" + ACCESS_TOKEN + "\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(0))
                .andExpect(jsonPath("$.tip").value(0))
                .andExpect(jsonPath("$.state").value(0))
                .andExpect(jsonPath("$.execute_after").value(0))
                .andExpect(jsonPath("$.blockers").doesNotExist());

        ArgumentCaptor<CancelAccountDeletionRequest> bound = ArgumentCaptor.forClass(CancelAccountDeletionRequest.class);
        verify(service).cancelDeletion(bound.capture());
        assertEquals(ZONE, bound.getValue().getZoneId());
        assertEquals(ACCESS_TOKEN, bound.getValue().getAccessToken());
        verify(limiter).check(eq(ZONE), eq(CLIENT_IP), eq(AssignGateController.cooldownKey(ACCESS_TOKEN)), eq("login"));
    }

    @Test
    void rateLimited_returns429WithoutCallingLogin() throws Exception {
        when(limiter.check(anyLong(), any(), any(), any())).thenReturn(RateLimitDecision.deny("ACCOUNT_COOLDOWN"));

        mvc.perform(post("/api/account/delete").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3,\"account\":\"alice\",\"password\":\"pw\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(429))
                .andExpect(jsonPath("$.message").value("ACCOUNT_COOLDOWN"))
                .andExpect(jsonPath("$.tip").value(0))
                .andExpect(jsonPath("$.state").doesNotExist());

        // 区桶空 / 波次未开:/api/login 回排队,注销没有排队语义,同样回 429。
        when(limiter.check(anyLong(), any(), any(), any())).thenReturn(RateLimitDecision.queue(2500, 0));

        mvc.perform(post("/api/account/delete/cancel").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3,\"access_token\":\"" + ACCESS_TOKEN + "\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(429))
                .andExpect(jsonPath("$.message").value("QUEUEING"));

        verifyNoInteractions(service);
    }

    @Test
    void invalidInput_rejectedAs401BeforeLimiterAndLogin() throws Exception {
        mvc.perform(post("/api/account/delete").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":0,\"account\":\"alice\",\"password\":\"pw\"}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(401))
                .andExpect(jsonPath("$.message").value("invalid_zone_id"))
                .andExpect(jsonPath("$.tip").value(0));
        mvc.perform(post("/api/account/delete/cancel").contentType(MediaType.APPLICATION_JSON)
                        .content("{\"zone_id\":3}"))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(401))
                .andExpect(jsonPath("$.message").value("empty_access_token"));

        verifyNoInteractions(limiter, service);
    }

    @Test
    void deleteValidation_zoneMustBeRoutableAndFieldsWithinLoginCaps() {
        assertEquals("invalid_zone_id", AccountDeletionController.invalidReason(deletion(0)));
        assertEquals("invalid_zone_id", AccountDeletionController.invalidReason(deletion(-1)));
        assertEquals("invalid_zone_id", AccountDeletionController.invalidReason(deletion(Integer.MAX_VALUE + 1L)));
        assertNull(AccountDeletionController.invalidReason(deletion(Integer.MAX_VALUE)));

        // 账号按码点数:191 个表情(382 个 UTF-16 单元)仍合法,与 login 的 191 个 rune 一致。
        AccountDeletionRequest req = deletion(ZONE);
        req.setAccount("😀".repeat(AccountDeletionController.MAX_ACCOUNT_CODE_POINTS));
        assertNull(AccountDeletionController.invalidReason(req));
        req.setAccount("a".repeat(AccountDeletionController.MAX_ACCOUNT_CODE_POINTS + 1));
        assertEquals("field_too_long", AccountDeletionController.invalidReason(req));

        req = deletion(ZONE);
        req.setPassword("p".repeat(AccountDeletionController.MAX_PASSWORD_CHARS + 1));
        assertEquals("field_too_long", AccountDeletionController.invalidReason(req));

        req = deletion(ZONE);
        req.setAuthToken("t".repeat(AccountDeletionController.MAX_TOKEN_CHARS));
        req.setAuthorizationCode("c".repeat(AccountDeletionController.MAX_TOKEN_CHARS));
        assertNull(AccountDeletionController.invalidReason(req), "16 KiB 的身份令牌 login 收,网关不能更严");
        req.setAuthorizationCode("c".repeat(AccountDeletionController.MAX_TOKEN_CHARS + 1));
        assertEquals("field_too_long", AccountDeletionController.invalidReason(req));
    }

    @Test
    void cancelValidation_requiresRoutableZoneAndBoundedToken() {
        CancelAccountDeletionRequest req = new CancelAccountDeletionRequest();
        req.setZoneId(ZONE);
        assertEquals("empty_access_token", AccountDeletionController.invalidReason(req));
        req.setAccessToken("   ");
        assertEquals("empty_access_token", AccountDeletionController.invalidReason(req));
        req.setAccessToken("t".repeat(AccountDeletionController.MAX_TOKEN_CHARS + 1));
        assertEquals("field_too_long", AccountDeletionController.invalidReason(req));
        req.setAccessToken(ACCESS_TOKEN);
        assertNull(AccountDeletionController.invalidReason(req));
        req.setZoneId(0);
        assertEquals("invalid_zone_id", AccountDeletionController.invalidReason(req));
    }

    private static AccountDeletionRequest deletion(long zoneId) {
        AccountDeletionRequest req = new AccountDeletionRequest();
        req.setZoneId(zoneId);
        req.setAccount("alice");
        req.setPassword("pw");
        return req;
    }
}
