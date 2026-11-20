package com.game.gateway.service;

import com.game.gateway.dto.LoginResponse;
import com.game.gateway.dto.RealNameRequest;
import com.game.gateway.dto.RealNameResponse;
import com.game.gateway.grpc.LoginRpcClient;
import com.game.table.tip.TipFaults;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.Mockito.when;

/**
 * {@link LoginService#submitRealName} 的结论映射(docs/design/real-name-anti-addiction.md §5.2):成功 0、令牌不能用 401、
 * 实名没通过 422 带 tip、故障 500。
 */
@ExtendWith(MockitoExtension.class)
class LoginServiceRealNameTest {

    @Mock
    private LoginRpcClient rpc;

    private LoginService service;

    @BeforeEach
    void setUp() {
        service = new LoginService(rpc);
    }

    private static RealNameRequest request() {
        RealNameRequest req = new RealNameRequest();
        req.setAccessToken("tok");
        req.setName("张三");
        req.setIdNumber("11010519491231002X");
        req.setZoneId(1);
        return req;
    }

    private RealNameResponse respond(Integer tip) {
        var rsp = new LoginRpcClient.SubmitRealNameResponseProto();
        rsp.errorCode = tip;
        rsp.antiAddiction = new LoginRpcClient.AntiAddictionProto();
        rsp.antiAddiction.minor = true;
        when(rpc.submitRealName(any(LoginRpcClient.SubmitRealNameRequestProto.class), anyInt())).thenReturn(rsp);
        return service.submitRealName(request());
    }

    @Test
    void successCarriesTheStatus() {
        RealNameResponse out = respond(null);
        assertEquals(RealNameResponse.CODE_OK, out.getCode());
        assertNull(out.getTip());
        assertTrue(out.getAntiAddiction().isMinor());
    }

    @Test
    void businessRejectionIs422WithTip() {
        int rejection = 2000; // kLoginAccountNotFound 不是故障码(LoginServiceTest 已钉住这个前提),这里只当「非故障码」用
        assertFalse(TipFaults.isFault(rejection));
        RealNameResponse out = respond(rejection);
        assertEquals(RealNameResponse.CODE_REJECTED, out.getCode());
        assertEquals(rejection, out.getTip());
    }

    @Test
    void faultTipIs500() {
        int fault = TipFaults.FAULTS.get(0).code();
        RealNameResponse out = respond(fault);
        assertEquals(LoginResponse.CODE_INTERNAL, out.getCode());
        assertEquals(fault, out.getTip());
    }

    @Test
    void rejectedTokenIs401() {
        when(rpc.submitRealName(any(LoginRpcClient.SubmitRealNameRequestProto.class), anyInt()))
                .thenThrow(new StatusRuntimeException(Status.UNAUTHENTICATED));
        assertEquals(LoginResponse.CODE_AUTH_REJECTED, service.submitRealName(request()).getCode());
    }

    @Test
    void transportFailureIs500() {
        when(rpc.submitRealName(any(LoginRpcClient.SubmitRealNameRequestProto.class), anyInt()))
                .thenThrow(new StatusRuntimeException(Status.UNAVAILABLE));
        RealNameResponse out = service.submitRealName(request());
        assertEquals(LoginResponse.CODE_INTERNAL, out.getCode());
        assertEquals("login_unavailable", out.getMessage());
    }
}
