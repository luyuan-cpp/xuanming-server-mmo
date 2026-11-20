package com.game.gateway.service;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.game.gateway.dto.AccountDeletionRequest;
import com.game.gateway.dto.AccountDeletionResponse;
import com.game.gateway.dto.CancelAccountDeletionRequest;
import com.game.gateway.dto.LoginRequest;
import com.game.gateway.dto.LoginResponse;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.table.tip.TipFaults;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.http.converter.json.Jackson2ObjectMapperBuilder;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link AccountDeletionService} 对 login 应答的映射(docs/design/account-deletion.md §5.2):业务码与 {@link LoginService}
 * 同一套,tip、state、execute_after、挂起清单原样带给客户端;另验 /api/login 带出的三个注销字段与它们的 JSON 名。
 *
 * <p>码值:2000(凭证无效)与 2039(注销预检未通过)是设计 §5.2 给客户端的 tip,网关只原样透传、不认具体码;
 * 2039 在 Tip.xlsx 导表之前还不在生成表里。故障码按码名从生成表取,不手写数字。
 */
@ExtendWith(MockitoExtension.class)
class AccountDeletionServiceTest {

    private static final int ZONE = 3;
    private static final int TIP_CREDENTIAL_INVALID = 2000;
    private static final int TIP_DELETION_BLOCKED = 2039;

    /** 与线上同一种 Jackson 配置(application.yaml 的 SNAKE_CASE)。 */
    private static final ObjectMapper JSON = Jackson2ObjectMapperBuilder.json()
            .propertyNamingStrategy(PropertyNamingStrategies.SNAKE_CASE).build();

    @Mock
    private LoginRpcClient rpc;

    private AccountDeletionService service;

    @BeforeEach
    void setUp() {
        service = new AccountDeletionService(rpc);
    }

    @Test
    void accepted_copiesStateAndExecuteAfter_andForwardsCredentialsByZone() {
        var upstream = new LoginRpcClient.AccountDeletionResponseProto();
        upstream.state = 1;
        upstream.executeAfter = 1_800_000_000L;
        when(rpc.requestAccountDeletion(any(), anyInt())).thenReturn(upstream);

        AccountDeletionResponse out = service.requestDeletion(appleRequest());

        assertEquals(LoginResponse.CODE_OK, out.getCode());
        assertEquals(0, out.getTip());
        assertNull(out.getMessage());
        assertEquals(1, out.getState());
        assertEquals(1_800_000_000L, out.getExecuteAfter());
        assertNull(out.getBlockers(), "空的挂起清单不出现在应答里");

        ArgumentCaptor<LoginRpcClient.AccountDeletionRequestProto> sent =
                ArgumentCaptor.forClass(LoginRpcClient.AccountDeletionRequestProto.class);
        verify(rpc).requestAccountDeletion(sent.capture(), eq(ZONE));
        assertEquals("apple", sent.getValue().authType);
        assertEquals("apple-identity-token", sent.getValue().authToken);
        assertEquals("apple-authorization-code", sent.getValue().authorizationCode);
        assertEquals("", sent.getValue().account);
        assertEquals("", sent.getValue().password);
    }

    @Test
    void blocked_keepsTipStateAndBlockers() {
        assertFalse(TipFaults.isFault(TIP_DELETION_BLOCKED), "前提:预检没过是业务拒绝,不是故障");
        var upstream = new LoginRpcClient.AccountDeletionResponseProto();
        upstream.errorCode = TIP_DELETION_BLOCKED;
        upstream.blockers.add(blocker(101L, "guild_leader"));
        upstream.blockers.add(blocker(102L, "role_listed"));
        when(rpc.requestAccountDeletion(any(), anyInt())).thenReturn(upstream);

        AccountDeletionResponse out = service.requestDeletion(appleRequest());

        assertEquals(LoginResponse.CODE_AUTH_REJECTED, out.getCode());
        assertEquals(TIP_DELETION_BLOCKED, out.getTip());
        assertEquals("upstream_err=" + TIP_DELETION_BLOCKED, out.getMessage());
        assertEquals(0, out.getState());
        assertEquals(2, out.getBlockers().size());
        assertEquals(101L, out.getBlockers().get(0).getPlayerId());
        assertEquals("guild_leader", out.getBlockers().get(0).getReason());
        assertEquals(102L, out.getBlockers().get(1).getPlayerId());
        assertEquals("role_listed", out.getBlockers().get(1).getReason());
    }

    @Test
    void faultTip_mapsToInternalAndKeepsTip() {
        int tip = LoginServiceTest.faultCode("LoginAuthProviderUnavailable");
        var upstream = new LoginRpcClient.AccountDeletionResponseProto();
        upstream.errorCode = tip;
        when(rpc.requestAccountDeletion(any(), anyInt())).thenReturn(upstream);

        AccountDeletionResponse out = service.requestDeletion(appleRequest());

        assertEquals(LoginResponse.CODE_INTERNAL, out.getCode());
        assertEquals(tip, out.getTip());
        assertEquals("upstream_err=" + tip, out.getMessage());
    }

    @Test
    void credentialRejected_mapsToAuthRejectedAndKeepsTip() {
        assertFalse(TipFaults.isFault(TIP_CREDENTIAL_INVALID), "前提:凭证无效不是故障码");
        var upstream = new LoginRpcClient.AccountDeletionResponseProto();
        upstream.errorCode = TIP_CREDENTIAL_INVALID;
        when(rpc.requestAccountDeletion(any(), anyInt())).thenReturn(upstream);

        AccountDeletionResponse out = service.requestDeletion(appleRequest());

        assertEquals(LoginResponse.CODE_AUTH_REJECTED, out.getCode());
        assertEquals(TIP_CREDENTIAL_INVALID, out.getTip());
    }

    @Test
    void unavailable_mapsToLoginUnavailableWithoutLoginFields() {
        when(rpc.requestAccountDeletion(any(), anyInt()))
                .thenThrow(new StatusRuntimeException(Status.UNAVAILABLE.withDescription("login down")));

        AccountDeletionResponse out = service.requestDeletion(appleRequest());

        assertEquals(LoginResponse.CODE_INTERNAL, out.getCode());
        assertEquals("login_unavailable", out.getMessage());
        assertEquals(0, out.getTip());
        assertNull(out.getState(), "没有 login 的结论就不给 state,免得被当成「正常」");
        assertNull(out.getExecuteAfter());
        assertNull(out.getBlockers());
    }

    @Test
    void cancel_forwardsAccessTokenByZoneAndReportsStateZero() {
        when(rpc.cancelAccountDeletion(any(), anyInt())).thenReturn(new LoginRpcClient.AccountDeletionResponseProto());
        CancelAccountDeletionRequest req = new CancelAccountDeletionRequest();
        req.setZoneId(ZONE);
        req.setAccessToken("SYNTHETIC_ACCESS_TOKEN");

        AccountDeletionResponse out = service.cancelDeletion(req);

        assertEquals(LoginResponse.CODE_OK, out.getCode());
        assertEquals(0, out.getState(), "撤销成功回 state = 0,而且要出现在应答里");
        assertEquals(0L, out.getExecuteAfter());
        ArgumentCaptor<LoginRpcClient.CancelAccountDeletionRequestProto> sent =
                ArgumentCaptor.forClass(LoginRpcClient.CancelAccountDeletionRequestProto.class);
        verify(rpc).cancelAccountDeletion(sent.capture(), eq(ZONE));
        assertEquals("SYNTHETIC_ACCESS_TOKEN", sent.getValue().accessToken);
    }

    @Test
    void cancel_unauthenticatedStatus_mapsToAuthRejected() {
        when(rpc.cancelAccountDeletion(any(), anyInt()))
                .thenThrow(new StatusRuntimeException(Status.UNAUTHENTICATED));
        CancelAccountDeletionRequest req = new CancelAccountDeletionRequest();
        req.setZoneId(ZONE);
        req.setAccessToken("SYNTHETIC_ACCESS_TOKEN");

        AccountDeletionResponse out = service.cancelDeletion(req);

        assertEquals(LoginResponse.CODE_AUTH_REJECTED, out.getCode());
        assertEquals("UNAUTHENTICATED", out.getMessage());
        assertNull(out.getState());
    }

    /** /api/login 成功时带出三个注销字段(JSON 名见设计 §5.2);失败 / 排队的应答不带。 */
    @Test
    void login_carriesDeletionFieldsInSnakeCase() {
        var upstream = new LoginRpcClient.LoginResponseProto();
        upstream.accessToken = "access";
        upstream.accountDeletionState = 1;
        upstream.accountDeletionExecuteAfter = 1_800_000_000L;
        upstream.previousAccountDeletedAt = 1_700_000_000L;
        when(rpc.login(any(LoginRpcClient.LoginRequestProto.class), anyInt())).thenReturn(upstream);
        LoginRequest req = new LoginRequest();
        req.setZoneId(ZONE);
        req.setAccount("u_001");
        req.setPassword("pw");

        JsonNode json = JSON.valueToTree(new LoginService(rpc).login(req));

        assertEquals(LoginResponse.CODE_OK, json.get("code").asInt());
        assertEquals(1, json.get("account_deletion_state").asInt());
        assertEquals(1_800_000_000L, json.get("account_deletion_execute_after").asLong());
        assertEquals(1_700_000_000L, json.get("previous_account_deleted_at").asLong());
        JsonNode failed = JSON.valueToTree(LoginResponse.error(LoginResponse.CODE_INTERNAL, "login_unavailable"));
        assertFalse(failed.has("account_deletion_state"));
        assertFalse(failed.has("account_deletion_execute_after"));
        assertFalse(failed.has("previous_account_deleted_at"));
    }

    private static AccountDeletionRequest appleRequest() {
        AccountDeletionRequest req = new AccountDeletionRequest();
        req.setZoneId(ZONE);
        req.setAuthType("apple");
        req.setAuthToken("apple-identity-token");
        req.setAuthorizationCode("apple-authorization-code");
        return req;
    }

    private static AccountDeletionResponse.Blocker blocker(long playerId, String reason) {
        AccountDeletionResponse.Blocker b = new AccountDeletionResponse.Blocker();
        b.setPlayerId(playerId);
        b.setReason(reason);
        return b;
    }
}
