package com.game.gateway.integration;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.dto.LoginRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.dto.RefreshTokenRequest;
import com.game.gateway.dto.RefreshTokenResponse;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.GatewayNodeApplication;
import com.game.gateway.config.AuthorityDatabaseConfiguration;
import com.game.table.tip.TipFaults;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.web.servlet.AutoConfigureMockMvc;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.mock.mockito.MockBean;
import org.springframework.boot.test.context.TestComponent;
import org.springframework.boot.autoconfigure.EnableAutoConfiguration;
import org.springframework.boot.autoconfigure.AutoConfigurationExcludeFilter;
import org.springframework.boot.autoconfigure.domain.EntityScan;
import org.springframework.boot.context.TypeExcludeFilter;
import org.springframework.context.annotation.ComponentScan;
import org.springframework.context.annotation.Configuration;
import org.springframework.context.annotation.FilterType;
import org.springframework.data.jpa.repository.config.EnableJpaRepositories;
import org.springframework.scheduling.annotation.EnableScheduling;
import org.springframework.http.MediaType;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.MvcResult;

import org.mockito.ArgumentCaptor;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/**
 * End-to-end integration test for the new HTTP login surface.
 *
 * <p>Spring loads the full gateway context (controllers + services + Jackson
 * snake_case + the rate limiter wiring), but we mock {@link LoginRpcClient}
 * so the test does not require a running go-zero login or a real network
 * channel. What we verify here:
 * <ul>
 *   <li>{@code /api/login} accepts a snake_case JSON body, hits the gRPC
 *       client, and surfaces tokens / errors back as JSON.</li>
 *   <li>{@code /api/refresh-token} rotates tokens through the same path.</li>
 *   <li>The response JSON keys match what the robot client expects (the
 *       robot's {@code httpLoginResponse} struct uses snake_case tags).</li>
 *   <li>Upstream UNAVAILABLE / DEADLINE_EXCEEDED degrade to {@code code=500}
 *       so the robot's fallback logic trips at the right boundary.</li>
 * </ul>
 *
 * <p>Why a real Spring context (not a slice test): the robot/gateway HTTP
 * contract has historically been broken by Jackson / serialization config
 * differences. A full-context test catches those, while a {@code WebMvcTest}
 * would silently pick up default Jackson and miss snake_case mismatches.
 */
@SpringBootTest(classes = LoginEndpointIntegrationTest.H2HttpApplication.class)
@AutoConfigureMockMvc
@ActiveProfiles("test")
class LoginEndpointIntegrationTest {

    /**
     * 只隔离此HTTP契约用例的外部SQL适配。控制器、服务、Jackson、仓库与事务仍真实装配，
     * 使用原test profile的H2；NORMAL围栏由其真实SQL合同另验，生产配置不加测试旁路。
     * 排除原Main以免其ComponentScan再次导入AuthorityDatabaseConfiguration。
     */
    @Configuration(proxyBeanMethods = false)
    @TestComponent
    @EnableAutoConfiguration
    @EnableScheduling
    @EntityScan("com.game.gateway.entity")
    @EnableJpaRepositories("com.game.gateway.repository")
    @ComponentScan(basePackages = "com.game.gateway", excludeFilters = {
            @ComponentScan.Filter(type = FilterType.CUSTOM, classes = TypeExcludeFilter.class),
            @ComponentScan.Filter(type = FilterType.CUSTOM, classes = AutoConfigurationExcludeFilter.class),
            @ComponentScan.Filter(type = FilterType.ASSIGNABLE_TYPE,
                    classes = {AuthorityDatabaseConfiguration.class, GatewayNodeApplication.class})
    })
    static class H2HttpApplication { }

    @Autowired private MockMvc mockMvc;
    @Autowired private ObjectMapper objectMapper;

    /**
     * Replace the real gRPC client with a Mockito mock so the test does not
     * need a running login.rpc backend. The constructor still runs (it walks
     * the configured endpoints), but no calls are issued because we override
     * {@code login(..., zoneId)} / {@code refreshToken()}.
     */
    @MockBean private LoginRpcClient loginRpcClient;

    @Test
    void loginEndpoint_returnsTokensOnSuccess() throws Exception {
        LoginRpcClient.LoginResponseProto upstream = new LoginRpcClient.LoginResponseProto();
        upstream.accessToken = "access-abc";
        upstream.refreshToken = "refresh-xyz";
        upstream.accessTokenExpire = 1_700_000_000L;
        upstream.refreshTokenExpire = 1_702_000_000L;
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);

        LoginRequest req = new LoginRequest();
        req.setZoneId(1);
        req.setAccount("u_001");
        req.setPassword("pw");

        MvcResult res = mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_OK))
                .andExpect(jsonPath("$.access_token").value("access-abc"))
                .andExpect(jsonPath("$.refresh_token").value("refresh-xyz"))
                .andExpect(jsonPath("$.access_token_expire").value(1_700_000_000L))
                .andExpect(jsonPath("$.refresh_token_expire").value(1_702_000_000L))
                .andReturn();

        // Sanity-check the body decodes with the same DTO the robot would parse.
        LoginResponse decoded = objectMapper.readValue(res.getResponse().getContentAsString(), LoginResponse.class);
        assertEquals("access-abc", decoded.getAccessToken());
    }

    @Test
    void loginEndpoint_upstreamErrorMapsTo401() throws Exception {
        LoginRpcClient.LoginResponseProto upstream = new LoginRpcClient.LoginResponseProto();
        upstream.errorCode = 42;
        upstream.errorMessage = "auth_provider_rejected";
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);

        LoginRequest req = new LoginRequest();
        req.setAuthType("wechat");
        req.setAuthToken("invalid-code");

        mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_AUTH_REJECTED))
                .andExpect(jsonPath("$.message").value("auth_provider_rejected"));
    }

    /**
     * 客户端契约（两版同批）：login 回 Tip.xlsx 标了 fault 的码时，JSON code 是 500 而不是 401，
     * HTTP 状态仍是 200，message 仍是 {@code upstream_err=<tip>}。码值按码名从生成表取，不手写数字。
     */
    @Test
    void loginEndpoint_upstreamFaultTipMapsTo500() throws Exception {
        int tip = TipFaults.FAULTS.stream()
                .filter(f -> f.name().equals("LoginAuthProviderUnavailable"))
                .findFirst()
                .orElseThrow()
                .code();
        LoginRpcClient.LoginResponseProto upstream = new LoginRpcClient.LoginResponseProto();
        upstream.errorCode = tip;
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);

        LoginRequest req = new LoginRequest();
        req.setAuthType("wechat");
        req.setAuthToken("code-while-wechat-is-down");

        mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_INTERNAL))
                .andExpect(jsonPath("$.message").value("upstream_err=" + tip));
    }

    @Test
    void loginEndpoint_unavailableMapsTo500() throws Exception {
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt()))
                .thenThrow(new StatusRuntimeException(Status.UNAVAILABLE.withDescription("login down")));

        LoginRequest req = new LoginRequest();
        req.setAccount("u_002");
        req.setPassword("pw");

        mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_INTERNAL))
                .andExpect(jsonPath("$.message").value("login_unavailable"));
    }

    @Test
    void refreshTokenEndpoint_rotatesTokens() throws Exception {
        LoginRpcClient.RefreshTokenResponseProto upstream = new LoginRpcClient.RefreshTokenResponseProto();
        upstream.accessToken = "new-access";
        upstream.refreshToken = "new-refresh";
        upstream.accessTokenExpire = 1_700_000_000L;
        upstream.refreshTokenExpire = 1_702_000_000L;
        when(loginRpcClient.refreshToken(any(LoginRpcClient.RefreshTokenRequestProto.class))).thenReturn(upstream);

        RefreshTokenRequest req = new RefreshTokenRequest();
        req.setRefreshToken("old-refresh");

        mockMvc.perform(post("/api/refresh-token")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(RefreshTokenResponse.CODE_OK))
                .andExpect(jsonPath("$.access_token").value("new-access"))
                .andExpect(jsonPath("$.refresh_token").value("new-refresh"));
    }

    /**
     * Regression guard for the third-party-login wire path.
     *
     * <p>Robot/SDK clients post {@code auth_type:"wechat"} with the OAuth
     * code in {@code auth_token}; we want both fields to survive the
     * snake_case ↔ camelCase ↔ protobuf-wire shuffle. If this ever
     * breaks (Jackson naming change, DTO rename, marshaller drop), the
     * provider on the go-zero side gets an empty token and silently
     * returns "wechat: empty code" — visible to the user only as a 401
     * with no clue why.
     *
     * <p>We capture the {@link LoginRpcClient.LoginRequestProto} that
     * actually got handed to the gRPC marshaller and assert each field
     * matches what the HTTP body carried. Same invariant applies to QQ /
     * SaToken / future providers, so the second case parameterizes on
     * type to keep the matrix honest.
     */
    @Test
    void loginEndpoint_propagatesAuthTypeAndAuthToken_wechat() throws Exception {
        LoginRpcClient.LoginResponseProto upstream = new LoginRpcClient.LoginResponseProto();
        upstream.accessToken = "wx-access";
        upstream.refreshToken = "wx-refresh";
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);

        LoginRequest req = new LoginRequest();
        req.setZoneId(1);
        req.setAuthType("wechat");
        req.setAuthToken("oauth-code-from-mp-sdk");
        req.setDeviceId("dev-1");

        mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_OK))
                .andExpect(jsonPath("$.access_token").value("wx-access"));

        ArgumentCaptor<LoginRpcClient.LoginRequestProto> captor =
                ArgumentCaptor.forClass(LoginRpcClient.LoginRequestProto.class);
        verify(loginRpcClient).login(captor.capture(), anyInt());
        LoginRpcClient.LoginRequestProto sent = captor.getValue();
        assertEquals("wechat", sent.authType, "auth_type lost on the way to gRPC");
        assertEquals("oauth-code-from-mp-sdk", sent.authToken, "auth_token lost on the way to gRPC");
        // For third-party auth the proto's `account` field is intentionally
        // empty — the provider resolves the account from the token itself.
        assertEquals("", sent.account, "account must be empty for third-party auth");
        assertEquals("", sent.password, "password must be empty for third-party auth");
    }

    @Test
    void loginEndpoint_propagatesAuthTypeAndAuthToken_qq() throws Exception {
        LoginRpcClient.LoginResponseProto upstream = new LoginRpcClient.LoginResponseProto();
        upstream.accessToken = "qq-access";
        upstream.refreshToken = "qq-refresh";
        when(loginRpcClient.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);

        LoginRequest req = new LoginRequest();
        req.setAuthType("qq");
        // QQ Connect hands the SDK an access_token (not an OAuth code) and
        // we forward exactly that string. Pin the value so a future
        // refactor that hashes/escapes/normalizes the token before send
        // surfaces the change loudly.
        req.setAuthToken("qq-platform-access-token-rawvalue");

        mockMvc.perform(post("/api/login")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(LoginResponse.CODE_OK));

        ArgumentCaptor<LoginRpcClient.LoginRequestProto> captor =
                ArgumentCaptor.forClass(LoginRpcClient.LoginRequestProto.class);
        verify(loginRpcClient).login(captor.capture(), anyInt());
        assertEquals("qq", captor.getValue().authType);
        assertEquals("qq-platform-access-token-rawvalue", captor.getValue().authToken);
    }

    @Test
    void refreshTokenEndpoint_emptyTokenRejectedWithoutRpc() throws Exception {
        RefreshTokenRequest req = new RefreshTokenRequest();
        req.setRefreshToken("");

        mockMvc.perform(post("/api/refresh-token")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(req)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(RefreshTokenResponse.CODE_AUTH_REJECTED))
                .andExpect(jsonPath("$.message").value("empty_refresh_token"));
    }
}
