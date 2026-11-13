package com.game.gateway.config;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletRequestWrapper;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.context.properties.bind.Bindable;
import org.springframework.boot.context.properties.bind.Binder;
import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.core.env.Environment;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;

import java.io.IOException;
import java.util.Collection;
import java.util.Collections;
import java.util.Enumeration;
import java.util.HashSet;
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
 * <p>藏的是 Spring Boot 能消费的三种传播格式(W3C、B3 单头、B3 多头)的全部头、W3C baggage,
 * 以及配置项 {@code management.tracing.baggage.remote-fields} 里的字段:这些字段按字段名原样作为请求头传播,
 * 同样会被观测层接收。名单在构造时从同一个配置项读出,配了新字段不用改这里。只藏不改:请求的其余部分原样往下传。
 */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
public class UntrustedTraceHeaderFilter extends OncePerRequestFilter {

    /** Spring Boot 能消费的传播头,全小写;HTTP 头名不区分大小写,比较前先转小写。 */
    static final Set<String> PROPAGATION_HEADERS = Set.of(
            "traceparent", "tracestate", "baggage",
            "b3",
            "x-b3-traceid", "x-b3-spanid", "x-b3-parentspanid", "x-b3-sampled", "x-b3-flags");

    /** Spring Boot 的远程 baggage 字段配置项:列出的字段按字段名原样作为请求头传播。 */
    static final String REMOTE_FIELDS_PROPERTY = "management.tracing.baggage.remote-fields";

    private final Set<String> hiddenHeaders;

    /** 从配置读远程 baggage 字段;逗号分隔与 YAML 列表两种写法都认,没配就只藏 {@link #PROPAGATION_HEADERS}。 */
    @Autowired
    public UntrustedTraceHeaderFilter(Environment environment) {
        this(Binder.get(environment).bind(REMOTE_FIELDS_PROPERTY, Bindable.listOf(String.class)).orElse(List.of()));
    }

    /** remoteBaggageFields 与 {@link #PROPAGATION_HEADERS} 一起藏掉;字段名不区分大小写,空白项忽略。 */
    UntrustedTraceHeaderFilter(Collection<String> remoteBaggageFields) {
        Set<String> names = new HashSet<>(PROPAGATION_HEADERS);
        for (String field : remoteBaggageFields) {
            if (field != null && !field.isBlank()) {
                names.add(field.trim().toLowerCase(Locale.ROOT));
            }
        }
        this.hiddenHeaders = Set.copyOf(names);
    }

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response, FilterChain chain)
            throws ServletException, IOException {
        chain.doFilter(new WithoutTraceHeaders(request, hiddenHeaders), response);
    }

    /** 这个头名会不会被藏掉。 */
    boolean isHidden(String name) {
        return isHidden(hiddenHeaders, name);
    }

    private static boolean isHidden(Set<String> hiddenHeaders, String name) {
        return name != null && hiddenHeaders.contains(name.toLowerCase(Locale.ROOT));
    }

    /** 读头的各个入口都按同一份名单藏;容器默认实现里 getIntHeader / getDateHeader 不经过 getHeader,要分别覆盖。 */
    private static final class WithoutTraceHeaders extends HttpServletRequestWrapper {

        private final Set<String> hiddenHeaders;

        WithoutTraceHeaders(HttpServletRequest request, Set<String> hiddenHeaders) {
            super(request);
            this.hiddenHeaders = hiddenHeaders;
        }

        @Override
        public String getHeader(String name) {
            return isHidden(hiddenHeaders, name) ? null : super.getHeader(name);
        }

        @Override
        public Enumeration<String> getHeaders(String name) {
            return isHidden(hiddenHeaders, name) ? Collections.emptyEnumeration() : super.getHeaders(name);
        }

        @Override
        public Enumeration<String> getHeaderNames() {
            Enumeration<String> all = super.getHeaderNames();
            if (all == null) {
                // Servlet 规范:容器不允许读头名时返回 null,原样传下去。
                return null;
            }
            List<String> names = Collections.list(all);
            names.removeIf(name -> isHidden(hiddenHeaders, name));
            return Collections.enumeration(names);
        }

        @Override
        public long getDateHeader(String name) {
            return isHidden(hiddenHeaders, name) ? -1L : super.getDateHeader(name);
        }

        @Override
        public int getIntHeader(String name) {
            return isHidden(hiddenHeaders, name) ? -1 : super.getIntHeader(name);
        }
    }
}
