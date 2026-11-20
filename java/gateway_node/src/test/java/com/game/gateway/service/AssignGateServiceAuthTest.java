package com.game.gateway.service;

import com.game.gateway.dto.AssignGateRequest;
import com.game.gateway.dto.AssignGateResponse;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.gateway.repository.ZoneConfigRepository;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import java.util.Optional;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * /api/assign-gate 的鉴权(2026-11-12):access token 原样转给 login,login 回 status=4(UNAUTHENTICATED)时网关回 code=401。
 * 4 不能落进 mapAssignGate 的 default 分支:那里按 ADMITTED 处理,会给客户端一个空地址。
 */
@ExtendWith(MockitoExtension.class)
class AssignGateServiceAuthTest {

    private static final String ACCESS_TOKEN = "SYNTHETIC_ACCESS_TOKEN_VALUE";

    @Mock
    private LoginRpcClient rpc;

    @Mock
    private ZoneConfigRepository zoneConfigRepo;

    private AssignGateService service;

    @BeforeEach
    void setUp() {
        service = new AssignGateService(rpc, zoneConfigRepo);
        ZoneConfig open = new ZoneConfig();
        open.setZoneId(1L);
        open.setName("zone-1");
        open.setManualStatus(0);
        when(zoneConfigRepo.findById(1L)).thenReturn(Optional.of(open));
    }

    @Test
    void forwardsAccessTokenToLogin() {
        var admitted = new LoginRpcClient.AssignGateResponseProto();
        admitted.status = 0;
        admitted.ip = "10.0.0.8";
        admitted.port = 18000;
        admitted.tokenPayload = new byte[] {1};
        admitted.tokenSignature = new byte[] {2};
        admitted.tokenDeadline = 123L;
        when(rpc.assignGate(any(), anyInt())).thenReturn(admitted);

        AssignGateResponse out = service.assignGate(request());

        ArgumentCaptor<LoginRpcClient.AssignGateRequestProto> sent = ArgumentCaptor.forClass(LoginRpcClient.AssignGateRequestProto.class);
        verify(rpc).assignGate(sent.capture(), anyInt());
        assertEquals(ACCESS_TOKEN, sent.getValue().accessToken, "access token 原样交给 login 鉴权");
        assertEquals(0, out.getCode());
    }

    @Test
    void unauthenticatedStatusMapsTo401WithoutAddress() {
        var denied = new LoginRpcClient.AssignGateResponseProto();
        denied.status = 4;
        denied.error = "access token invalid";
        when(rpc.assignGate(any(), anyInt())).thenReturn(denied);

        AssignGateResponse out = service.assignGate(request());

        assertEquals(401, out.getCode());
        assertEquals("access token invalid", out.getError());
        assertNull(out.getGateIp(), "不能给出 gate 地址");
        assertFalse(String.valueOf(out.getError()).contains(ACCESS_TOKEN), "回包不回显令牌");
    }

    @Test
    void unauthenticatedWithoutReasonStillHasStableError() {
        var denied = new LoginRpcClient.AssignGateResponseProto();
        denied.status = 4;
        when(rpc.assignGate(any(), anyInt())).thenReturn(denied);

        AssignGateResponse out = service.assignGate(request());

        assertEquals(401, out.getCode());
        assertEquals("unauthenticated", out.getError());
    }

    /**
     * login 回 status=5(本区队列已到上限,2026-11-20)时,网关回前置限流那种重试提示:没有排队令牌、没有 gate 地址。
     * 5 同样不能落进 mapAssignGate 的 default 分支。
     */
    @Test
    void queueFullMapsToTheRateLimitRetryHintWithoutTokenOrAddress() {
        var full = new LoginRpcClient.AssignGateResponseProto();
        full.status = 5;
        full.retryAfterMs = 34_000;
        when(rpc.assignGate(any(), anyInt())).thenReturn(full);

        AssignGateResponse out = service.assignGate(request());

        assertEquals(100, out.getCode());
        assertEquals("ratelimit", out.getQueueSource(), "客户端按无票重试处理,不当成真排位");
        assertEquals(34_000L, out.getRetryAfterMs());
        assertNull(out.getQueueToken(), "没有入队,不能给排队令牌");
        assertNull(out.getGateIp(), "不能给出 gate 地址");
    }

    @Test
    void queueFullWithoutHintFallsBackToThirtySeconds() {
        var full = new LoginRpcClient.AssignGateResponseProto();
        full.status = 5;
        when(rpc.assignGate(any(), anyInt())).thenReturn(full);

        AssignGateResponse out = service.assignGate(request());

        assertEquals(100, out.getCode());
        assertEquals(30_000L, out.getRetryAfterMs());
    }

    /** 小游戏客户端的 WebSocket 地址原样透传(docs/design/minigame-websocket-gate.md W7);gate 没自报时不出现在应答里。 */
    @Test
    void gateWsUrlIsPassedThroughOnlyWhenTheGateAdvertisesOne() {
        var admitted = new LoginRpcClient.AssignGateResponseProto();
        admitted.status = 0;
        admitted.ip = "10.0.0.8";
        admitted.port = 18000;
        admitted.wsUrl = "wss://ws.example.com/ws/10.244.0.8/18000";
        when(rpc.assignGate(any(), anyInt())).thenReturn(admitted);

        assertEquals("wss://ws.example.com/ws/10.244.0.8/18000", service.assignGate(request()).getGateWsUrl());

        admitted.wsUrl = "";
        assertNull(service.assignGate(request()).getGateWsUrl(), "空地址不下发,JSON 里不出现 gate_ws_url");
    }

    private static AssignGateRequest request() {
        AssignGateRequest req = new AssignGateRequest();
        req.setZoneId(1);
        req.setAccessToken(ACCESS_TOKEN);
        req.setDeviceId("dev");
        return req;
    }
}
