package com.game.gateway.config;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletRequestWrapper;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;

import java.io.IOException;
import java.util.Collections;
import java.util.Enumeration;
import java.util.List;
import java.util.Locale;
import java.util.Set;

/**
 * 公网 HTTP 入口不采信客户端带来的链路追踪头(docs/design/distributed-tracing.md §3.3、§7)。
 *
 * <p>网关的 HTTP 接口都对公网开放。Micrometer Tracing 默认会接上请求里的 {@code traceparent}(也认 B3 头),
 * 那样客户端就能指定 trace_id:把自己的请求挂进别人的链路,或者让日志按它给的 id 聚到一起。
 * 这里把 Spring Boot 能消费的传播头从请求里藏掉,观测层读不到,每个请求都开新根;出站 gRPC 照常带网关自己的 traceparent。
 *
 * <p>必须排在 Spring Boot 注册的 {@code ServerHttpObservationFilter}(顺序 {@code Ordered.HIGHEST_PRECEDENCE + 1})之前,
 * 它在那里从请求头提取上下文,所以本过滤器取 {@code HIGHEST_PRECEDENCE}。
 *
 * <p>藏的是 Spring Boot 能消费的三种传播格式(W3C、B3 单头、B3 多头)的全部头,外加 W3C baggage。
 * 以后若配置 {@code management.tracing.baggage.remote-fields},那些字段名也要加进 {@link #TRACE_HEADERS}。
 * 只藏不改:请求的其余部分原样往下传。
 */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
public class UntrustedTraceHeaderFilter extends OncePerRequestFilter {

    /** 全小写;HTTP 头名不区分大小写,比较前先转小写。 */
    static final Set<String> TRACE_HEADERS = Set.of(
            "traceparent", "tracestate", "baggage",
            "b3",
            "x-b3-traceid", "x-b3-spanid", "x-b3-parentspanid", "x-b3-sampled", "x-b3-flags");

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response, FilterChain chain)
            throws ServletException, IOException {
        chain.doFilter(new WithoutTraceHeaders(request), response);
    }

    static boolean isTraceHeader(String name) {
        return name != null && TRACE_HEADERS.contains(name.toLowerCase(Locale.ROOT));
    }

    /** 读头的各个入口都按同一份名单藏;容器默认实现里 getIntHeader / getDateHeader 不经过 getHeader,要分别覆盖。 */
    private static final class WithoutTraceHeaders extends HttpServletRequestWrapper {

        WithoutTraceHeaders(HttpServletRequest request) {
            super(request);
        }

        @Override
        public String getHeader(String name) {
            return isTraceHeader(name) ? null : super.getHeader(name);
        }

        @Override
        public Enumeration<String> getHeaders(String name) {
            return isTraceHeader(name) ? Collections.emptyEnumeration() : super.getHeaders(name);
        }

        @Override
        public Enumeration<String> getHeaderNames() {
            Enumeration<String> all = super.getHeaderNames();
            if (all == null) {
                // Servlet 规范:容器不允许读头名时返回 null,原样传下去。
                return null;
            }
            List<String> names = Collections.list(all);
            names.removeIf(UntrustedTraceHeaderFilter::isTraceHeader);
            return Collections.enumeration(names);
        }

        @Override
        public long getDateHeader(String name) {
            return isTraceHeader(name) ? -1L : super.getDateHeader(name);
        }

        @Override
        public int getIntHeader(String name) {
            return isTraceHeader(name) ? -1 : super.getIntHeader(name);
        }
    }
}
