package com.game.gateway.controller;

import com.game.gateway.grpc.LoginRpcClient;
import io.grpc.Status;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;
import org.springframework.mock.web.MockHttpServletRequest;

import java.io.IOException;
import java.nio.charset.StandardCharsets;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.AdditionalMatchers.aryEq;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

/**
 * 钉住官方通行证回调入口(docs/design/official-passport-registration.md §3.7)的口径:没配密钥就关着;密钥不对不转发、
 * 也不读请求体;请求体原样转给 login;状态码给 Casdoor 的重投用(400 不重投、503 重投)。
 */
class PassportEventControllerTest {

    private static final String SECRET = "0123456789abcdef0123456789abcdef";
    private static final byte[] BODY = "{\"organization\":\"xuanming\",\"user\":\"alice\"}".getBytes(StandardCharsets.UTF_8);

    @Test
    void closedWithoutASecret() throws IOException {
        LoginRpcClient login = mock(LoginRpcClient.class);
        assertEquals(HttpStatus.NOT_FOUND, new PassportEventController(login, "").handle(SECRET, post(BODY)).getStatusCode());
        verifyNoInteractions(login);
    }

    @Test
    void rejectsShortSecretsAtStartup() {
        assertThrows(IllegalStateException.class, () -> new PassportEventController(mock(LoginRpcClient.class), "short"));
    }

    @Test
    void rejectsWrongSecretsAndBadBodiesWithoutCallingLogin() throws IOException {
        LoginRpcClient login = mock(LoginRpcClient.class);
        PassportEventController controller = new PassportEventController(login, SECRET);
        assertEquals(HttpStatus.UNAUTHORIZED, controller.handle(null, post(BODY)).getStatusCode());
        assertEquals(HttpStatus.UNAUTHORIZED, controller.handle(SECRET + "x", post(BODY)).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST, controller.handle(SECRET, post(new byte[0])).getStatusCode());
        assertEquals(HttpStatus.BAD_REQUEST,
                controller.handle(SECRET, post(new byte[PassportEventController.MAX_BODY_BYTES + 1])).getStatusCode());
        verifyNoInteractions(login);
    }

    @Test
    void readsNothingBeforeTheSecretChecksOut() throws IOException {
        PassportEventController controller = new PassportEventController(mock(LoginRpcClient.class), SECRET);
        MockHttpServletRequest huge = post(new byte[PassportEventController.MAX_BODY_BYTES * 4]);
        assertEquals(HttpStatus.UNAUTHORIZED, controller.handle("wrong", huge).getStatusCode());
        assertEquals(PassportEventController.MAX_BODY_BYTES * 4, huge.getInputStream().readAllBytes().length,
                "the body must stay unread when the secret is wrong");
    }

    @Test
    void forwardsTheBodyAndMapsLoginOutcomes() throws IOException {
        LoginRpcClient login = mock(LoginRpcClient.class);
        when(login.handlePassportEvent(any())).thenReturn(new LoginRpcClient.PassportEventResponseProto());
        PassportEventController controller = new PassportEventController(login, SECRET);
        assertEquals(HttpStatus.OK, controller.handle(SECRET, post(BODY)).getStatusCode());
        verify(login).handlePassportEvent(aryEq(BODY));

        reset(login);
        when(login.handlePassportEvent(any())).thenThrow(Status.INVALID_ARGUMENT.asRuntimeException());
        assertEquals(HttpStatus.BAD_REQUEST, controller.handle(SECRET, post(BODY)).getStatusCode());

        reset(login);
        when(login.handlePassportEvent(any())).thenThrow(Status.UNAVAILABLE.asRuntimeException());
        assertEquals(HttpStatus.SERVICE_UNAVAILABLE, controller.handle(SECRET, post(BODY)).getStatusCode());
    }

    private static MockHttpServletRequest post(byte[] body) {
        MockHttpServletRequest request = new MockHttpServletRequest("POST", "/internal/passport/events");
        request.setContentType("application/json");
        request.setContent(body);
        return request;
    }
}
