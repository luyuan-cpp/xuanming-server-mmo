package com.game.config.controller;

import com.game.config.service.TableService;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;
import org.springframework.mock.web.MockHttpServletRequest;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.mockito.Mockito.*;

class ReloadControllerTest {

    @Test
    void rejectsMissingOrWrongApiKeyWhenConfigured() {
        TableService tables = mock(TableService.class);
        ReloadController controller = new ReloadController(tables, "secret-key-123");
        MockHttpServletRequest req = new MockHttpServletRequest();
        req.setRemoteAddr("127.0.0.1");

        assertEquals(HttpStatus.UNAUTHORIZED, controller.reload(null, req).getStatusCode());
        assertEquals(HttpStatus.UNAUTHORIZED, controller.reload("wrong", req).getStatusCode());
        verify(tables, never()).reload();

        when(tables.getLoadVersion()).thenReturn(2L);
        when(tables.getLastLoadTimeMs()).thenReturn(15L);
        assertEquals(HttpStatus.OK, controller.reload("secret-key-123", req).getStatusCode());
        verify(tables, times(1)).reload();
    }

    @Test
    void withoutApiKeyAllowsDirectLoopbackAndRejectsRemoteOrForwarded() {
        TableService tables = mock(TableService.class);
        ReloadController controller = new ReloadController(tables, "");

        MockHttpServletRequest remote = new MockHttpServletRequest();
        remote.setRemoteAddr("10.0.0.8");
        assertEquals(HttpStatus.FORBIDDEN, controller.reload(null, remote).getStatusCode());

        MockHttpServletRequest forwarded = new MockHttpServletRequest();
        forwarded.setRemoteAddr("127.0.0.1");
        forwarded.addHeader("X-Forwarded-For", "203.0.113.5");
        assertEquals(HttpStatus.FORBIDDEN, controller.reload(null, forwarded).getStatusCode());
        verify(tables, never()).reload();

        MockHttpServletRequest local = new MockHttpServletRequest();
        local.setRemoteAddr("127.0.0.1");
        when(tables.getLoadVersion()).thenReturn(1L);
        when(tables.getLastLoadTimeMs()).thenReturn(8L);
        assertEquals(HttpStatus.OK, controller.reload(null, local).getStatusCode());
        verify(tables, times(1)).reload();
    }
}
