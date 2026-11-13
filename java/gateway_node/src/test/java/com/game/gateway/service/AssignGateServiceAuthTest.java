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

    private static AssignGateRequest request() {
        AssignGateRequest req = new AssignGateRequest();
        req.setZoneId(1);
        req.setAccessToken(ACCESS_TOKEN);
        req.setDeviceId("dev");
        return req;
    }
}
