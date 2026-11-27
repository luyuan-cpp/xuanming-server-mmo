package com.game.gateway.controller;

import com.game.gateway.grpc.LoginRpcClient;
import io.grpc.Status;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;

import java.nio.charset.StandardCharsets;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

/**
 * 钉住官方通行证回调入口(docs/design/official-passport-registration.md §3.7)的口径:没配密钥就关着;密钥不对不转发;
 * 请求体原样转给 login;状态码给 Casdoor 的重投用(400 不重投、503 重投)。
 */
class PassportEventControllerTest {

    private static final String SECRET = "0123456789abcdef0123456789abcdef";
    private static final byte[] BODY = "{\"organization\":\"xuanming\",\"user\":\"alice\"}".getBytes(StandardCharsets.UTF_8);

    @Test
    void closedWithoutASecret() {
        LoginRpcClient login = mock(LoginRpcClient.class);
        assertEquals(HttpStatus.NOT_FOUND, new PassportEventController(login, "").handle(SECRET, BODY).getStatusCode());
        verifyNoInteractions(login);
    }

    @Test
    void rejectsShortSecretsAtStartup() {
        assertThrows(IllegalStateException.class, () -> new PassportEventController(mock(LoginRpcClient.class), "short"));
    }

    @Test
    void rejectsWrongSecretsAndBadBodiesWithoutCallingLogin() {
        LoginRpcClient login = mock(LoginRpcClient.class);
        PassportEventController controller = new PassportEventController(login, SECRET);
        assertEquals(HttpStatus.UNAUTHORIZED, controller.handle(null, BODY).getStatusCode());
        assertEquals(HttpStatus.UNAUTHORIZED, controller.handle(SECRET + "x", BODY).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST, controller.handle(SECRET, new byte[0]).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST,
                controller.handle(SECRET, new byte[PassportEventController.MAX_BODY_BYTES + 1]).getStatusCode());
        verifyNoInteractions(login);
    }

    @Test
    void forwardsTheBodyAndMapsLoginOutcomes() {
        LoginRpcClient login = mock(LoginRpcClient.class);
        when(login.handlePassportEvent(any())).thenReturn(new LoginRpcClient.PassportEventResponseProto());
        PassportEventController controller = new PassportEventController(login, SECRET);
        assertEquals(HttpStatus.OK, controller.handle(SECRET, BODY).getStatusCode());
        verify(login).handlePassportEvent(BODY);

        reset(login);
        when(login.handlePassportEvent(any())).thenThrow(Status.INVALID_ARGUMENT.asRuntimeException());
        assertEquals(HttpStatus.BAD_REQUEST, controller.handle(SECRET, BODY).getStatusCode());

        reset(login);
        when(login.handlePassportEvent(any())).thenThrow(Status.UNAVAILABLE.asRuntimeException());
        assertEquals(HttpStatus.SERVICE_UNAVAILABLE, controller.handle(SECRET, BODY).getStatusCode());
    }
}
