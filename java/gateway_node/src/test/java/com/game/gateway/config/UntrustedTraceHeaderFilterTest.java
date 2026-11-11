package com.game.gateway.config;

import jakarta.servlet.http.HttpServletRequest;
import org.junit.jupiter.api.Test;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;

import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 公网入口藏掉客户端带来的追踪头(docs/design/distributed-tracing.md §3.3、§7)。
 * 不起 Spring:只看过滤器交给下游的请求还读不读得到这些头;与观测过滤器的先后顺序由 GatewayTracingIntegrationTest 覆盖。
 */
class UntrustedTraceHeaderFilterTest {

    private static final List<String> TRACE_HEADERS_AS_SENT = List.of(
            "traceparent", "TraceState", "baggage", "b3",
            "X-B3-TraceId", "X-B3-SpanId", "X-B3-ParentSpanId", "X-B3-Sampled", "X-B3-Flags");

    @Test
    void downstreamCannotReadAnyTracePropagationHeader() throws Exception {
        HttpServletRequest downstream = filter(requestWithTraceHeaders());

        for (String name : TRACE_HEADERS_AS_SENT) {
            assertNull(downstream.getHeader(name), name);
            // 头名不区分大小写,换一种写法也读不到。
            assertNull(downstream.getHeader(name.toUpperCase(Locale.ROOT)), name);
            assertFalse(downstream.getHeaders(name).hasMoreElements(), name);
            assertEquals(-1, downstream.getIntHeader(name), name);
            assertEquals(-1L, downstream.getDateHeader(name), name);
        }
        List<String> names = Collections.list(downstream.getHeaderNames());
        assertTrue(names.stream().noneMatch(UntrustedTraceHeaderFilter::isTraceHeader), names.toString());
    }

    @Test
    void otherHeadersPassThroughUnchanged() throws Exception {
        HttpServletRequest downstream = filter(requestWithTraceHeaders());

        assertEquals("203.0.113.7", downstream.getHeader("X-Forwarded-For"));
        assertEquals("application/json", downstream.getHeader("Content-Type"));
        assertEquals(42, downstream.getIntHeader("X-Retry-Count"));
        List<String> names = Collections.list(downstream.getHeaderNames());
        assertTrue(names.contains("X-Forwarded-For"), names.toString());
        assertTrue(names.contains("Content-Type"), names.toString());
        assertTrue(names.contains("X-Retry-Count"), names.toString());
    }

    private static MockHttpServletRequest requestWithTraceHeaders() {
        MockHttpServletRequest request = new MockHttpServletRequest("POST", "/api/login");
        request.addHeader("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01");
        request.addHeader("TraceState", "vendor=1");
        request.addHeader("baggage", "account=1");
        request.addHeader("b3", "4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-1");
        request.addHeader("X-B3-TraceId", "4bf92f3577b34da6a3ce929d0e0e4736");
        request.addHeader("X-B3-SpanId", "00f067aa0ba902b7");
        request.addHeader("X-B3-ParentSpanId", "00f067aa0ba902b7");
        request.addHeader("X-B3-Sampled", "1");
        request.addHeader("X-B3-Flags", "1");
        request.addHeader("X-Forwarded-For", "203.0.113.7");
        request.addHeader("Content-Type", "application/json");
        request.addHeader("X-Retry-Count", "42");
        return request;
    }

    /** 跑一次过滤器,返回它交给下游的请求。 */
    private static HttpServletRequest filter(MockHttpServletRequest request) throws Exception {
        AtomicReference<HttpServletRequest> downstream = new AtomicReference<>();
        new UntrustedTraceHeaderFilter().doFilter(request, new MockHttpServletResponse(),
                (req, res) -> downstream.set((HttpServletRequest) req));
        assertNotNull(downstream.get(), "filter must pass the request on");
        return downstream.get();
    }
}
