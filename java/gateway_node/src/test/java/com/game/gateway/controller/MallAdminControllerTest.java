package com.game.gateway.controller;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.config.MallAdminProperties;
import com.game.gateway.service.MallAdminClient;
import io.micrometer.observation.ObservationRegistry;
import jakarta.servlet.DispatcherType;
import jakarta.servlet.RequestDispatcher;
import jakarta.servlet.http.MappingMatch;
import java.nio.charset.StandardCharsets;
import java.util.List;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.http.HttpMethod;
import org.springframework.http.MediaType;
import org.springframework.mock.web.MockHttpServletMapping;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.request.MockHttpServletRequestBuilder;
import org.springframework.test.web.servlet.request.MockMvcRequestBuilders;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

/** 只装本控制器；不借全工程filter、数据库或发现服务替代方法内鉴权。 */
class MallAdminControllerTest {
    private static final String KEY = "test-only-gateway-admin-token-0000000001";
    private static final String MALL_SECRET = "test-only-upstream-signing-secret-000001";
    private static final List<String> COMMANDS = List.of("skus/list", "skus/create", "skus/update", "skus/stock", "order-view");
    private static final List<Prefix> PREFIXES = List.of(new Prefix("", ""), new Prefix("/ops", ""), new Prefix("/ops", "/api"));
    private static final byte[] BODY = bytes("{\n \"requestId\":\"controller-original-001\", \"skuId\":11,"
        + " \"expectedRevision\":2, \"terms\":{\"priceMinor\":125,\"title\":\"玄铁\"}\n}");

    @Test void context和servlet前缀及forward都拒绝缺失重复错误或过长令牌_不调用上游() throws Exception {
        var client = mock(MallAdminClient.class);
        var mvc = mvc(client, KEY);
        for (var prefix : PREFIXES) {
            for (var dispatcher : List.of(DispatcherType.REQUEST, DispatcherType.FORWARD)) {
                mvc.perform(command("skus/create", prefix, dispatcher)).andExpect(status().isUnauthorized());
                mvc.perform(command("skus/create", prefix, dispatcher).header("X-Admin-Key", KEY, KEY))
                    .andExpect(status().isUnauthorized());
                mvc.perform(command("skus/create", prefix, dispatcher).header("X-Admin-Key", "wrong-token"))
                    .andExpect(status().isUnauthorized());
                mvc.perform(command("skus/create", prefix, dispatcher).header("X-Admin-Key", "x".repeat(257)))
                    .andExpect(status().isUnauthorized());
                mvc.perform(command("skus/create", prefix, dispatcher).header("X-Admin-Key", KEY + "," + KEY))
                    .andExpect(status().isUnauthorized());
            }
        }
        verifyNoInteractions(client);
    }

    @Test void 未配置或短运维密钥一律关闭管理命令_即使请求令牌相同也不调用上游() throws Exception {
        for (String key : new String[] {null, "", "short", "x".repeat(31)}) {
            var client = mock(MallAdminClient.class);
            var mvc = mvc(client, key);
            for (var dispatcher : List.of(DispatcherType.REQUEST, DispatcherType.FORWARD)) {
                mvc.perform(command("skus/stock", new Prefix("/ops", "/api"), dispatcher)
                        .header("X-Admin-Key", key == null || key.isEmpty() ? KEY : key))
                    .andExpect(status().isServiceUnavailable());
            }
            verifyNoInteractions(client);
        }
    }

    @Test void 合法令牌的固定命令保留原始请求字节和上游状态正文_前缀与forward不改命令() throws Exception {
        var client = mock(MallAdminClient.class);
        byte[] response = bytes("{ \"error\":\"revision-conflict\", \"skuId\":11, \"lockedStock\":2 }");
        when(client.execute(anyString(), any(byte[].class))).thenReturn(new MallAdminClient.Reply(409, response));
        var mvc = mvc(client, KEY);
        for (String name : COMMANDS) {
            for (var dispatcher : List.of(DispatcherType.REQUEST, DispatcherType.FORWARD)) {
                mvc.perform(command(name, new Prefix("/ops", "/api"), dispatcher).header("X-Admin-Key", KEY))
                    .andExpect(status().isConflict())
                    .andExpect(header().string("Cache-Control", "no-store"))
                    .andExpect(content().contentType(MediaType.APPLICATION_JSON))
                    .andExpect(content().bytes(response));
            }
            verify(client, times(2)).execute(eq(name), any(byte[].class));
        }
        var bodies = ArgumentCaptor.forClass(byte[].class);
        verify(client, times(10)).execute(anyString(), bodies.capture());
        for (byte[] forwarded : bodies.getAllValues()) { assertArrayEquals(BODY, forwarded); }
        verifyNoMoreInteractions(client);
    }

    @Test void 未知路径查询串或超大请求体不进入通用代理() throws Exception {
        var client = mock(MallAdminClient.class);
        var mvc = mvc(client, KEY);
        var prefix = new Prefix("/ops", "/api");
        mvc.perform(command("skus/delete", prefix, DispatcherType.REQUEST).header("X-Admin-Key", KEY))
            .andExpect(status().isNotFound());
        mvc.perform(command("skus/create", prefix, DispatcherType.FORWARD).header("X-Admin-Key", KEY)
                .queryParam("target", "http://127.0.0.1/elsewhere"))
            .andExpect(status().isBadRequest());
        mvc.perform(command("skus/create", prefix, DispatcherType.REQUEST).header("X-Admin-Key", KEY)
                .content(new byte[16_385]))
            .andExpect(status().isPayloadTooLarge());
        verifyNoInteractions(client);
    }

    @Test void 页面仅提供静态界面和受限CSP_不嵌入网关或商城签名密钥() throws Exception {
        var props = new MallAdminProperties();
        props.setSecret(MALL_SECRET);
        props.validate();
        // 真配置包含本例签名材料；目标保持禁用，页面也不得调用HTTP适配器。
        var client = spy(new MallAdminClient(props, new ObjectMapper(), ObservationRegistry.NOOP));
        try {
            var mvc = mvc(client, KEY);
            for (var prefix : PREFIXES) {
                var result = mvc.perform(route(HttpMethod.GET, "/mall-admin", prefix, DispatcherType.REQUEST))
                    .andExpect(status().isOk())
                    .andExpect(content().contentTypeCompatibleWith(MediaType.TEXT_HTML))
                    .andExpect(header().string("Cache-Control", "no-store"))
                    .andExpect(header().string("X-Content-Type-Options", "nosniff"))
                    .andReturn();
                String csp = result.getResponse().getHeader("Content-Security-Policy");
                assertNotNull(csp);
                for (String directive : List.of("default-src 'none'", "script-src 'self'", "style-src 'self'",
                        "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'")) {
                    assertTrue(csp.contains(directive), directive);
                }
                assertFalse(csp.contains("unsafe-inline"));
                assertFalse(csp.contains("unsafe-eval"));
                String page = result.getResponse().getContentAsString(StandardCharsets.UTF_8);
                assertFalse(page.isBlank());
                assertFalse(page.contains(KEY));
                assertFalse(page.contains(MALL_SECRET));
            }
            verifyNoInteractions(client);
        } finally { client.shutdown(); }
    }

    private static MockMvc mvc(MallAdminClient client, String key) {
        return MockMvcBuilders.standaloneSetup(new MallAdminController(client, key)).build();
    }

    private static MockHttpServletRequestBuilder command(String name, Prefix prefix, DispatcherType dispatcher) {
        return route(HttpMethod.POST, "/admin/mall/" + name, prefix, dispatcher)
            .contentType(MediaType.APPLICATION_JSON).content(BODY);
    }

    private static MockHttpServletRequestBuilder route(HttpMethod method, String path, Prefix prefix, DispatcherType dispatcher) {
        var builder = MockMvcRequestBuilders.request(method, prefix.context() + prefix.servlet() + path)
            .contextPath(prefix.context());
        if (prefix.servlet().isEmpty()) {
            builder.servletPath(path);
        } else {
            builder.servletPath(prefix.servlet()).pathInfo(path).with(request -> {
                request.setHttpServletMapping(new MockHttpServletMapping(path.substring(1), prefix.servlet() + "/*",
                    "dispatcherServlet", MappingMatch.PATH));
                return request;
            });
        }
        return builder.with(request -> {
            request.setDispatcherType(dispatcher);
            if (dispatcher == DispatcherType.FORWARD) {
                request.setAttribute(RequestDispatcher.FORWARD_REQUEST_URI, prefix.context() + "/public-entry");
                request.setAttribute(RequestDispatcher.FORWARD_CONTEXT_PATH, prefix.context());
                request.setAttribute(RequestDispatcher.FORWARD_SERVLET_PATH, "/public-entry");
            }
            return request;
        });
    }

    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }
    private record Prefix(String context, String servlet) {}
}
