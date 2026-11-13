package com.game.gateway.config;

import jakarta.servlet.*;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Locale;

/**
 * 管理面(/admin/**)口令闸:请求头 X-Admin-Key 必须等于 admin.api-key。
 *
 * <p>2026-11-12 三处加固(热关停管理接口 /admin/killswitch 也挂在这道闸后面,docs/design/rpc-kill-switch.md §3):
 * <ul>
 *   <li>是不是管理路径,同时按容器规范化后的路径(servletPath + pathInfo:已解码、已去掉 ;路径参数)与原始 URI 判断,
 *       原始 URI 先去掉 ;路径参数、合并连续斜杠、转小写。旧写法只看原始 URI 前缀,{@code /admin;x=1/...} 这类请求
 *       不以 {@code /admin/} 开头而跳过口令,Spring 匹配时却会去掉路径参数、照样进到控制器;</li>
 *   <li>口令按 SHA-256 摘要做常量时间比较,不再用 equals(按字节提前返回,泄漏前缀匹配长度);</li>
 *   <li>没配口令(空白)时拒绝全部管理请求(fail-closed),不再出现「空口令 + 空请求头」放行。</li>
 * </ul>
 */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
public class AdminApiKeyFilter implements Filter {

    private static final String HEADER = "X-Admin-Key";
    private static final String ADMIN_PREFIX = "/admin";

    private final byte[] expectedDigest;

    public AdminApiKeyFilter(@Value("${admin.api-key}") String apiKey) {
        this.expectedDigest = apiKey == null || apiKey.isBlank() ? null : sha256(apiKey);
    }

    @Override
    public void doFilter(ServletRequest request, ServletResponse response, FilterChain chain)
            throws IOException, ServletException {

        HttpServletRequest httpReq = (HttpServletRequest) request;
        if (!isAdminPath(httpReq)) {
            chain.doFilter(request, response);
            return;
        }

        if (authorized(httpReq.getHeader(HEADER))) {
            chain.doFilter(request, response);
        } else {
            HttpServletResponse httpResp = (HttpServletResponse) response;
            httpResp.setStatus(HttpServletResponse.SC_UNAUTHORIZED);
            httpResp.setContentType("application/json");
            httpResp.getWriter().write("{\"error\":\"Unauthorized\"}");
        }
    }

    boolean authorized(String provided) {
        if (expectedDigest == null || provided == null) {
            return false;
        }
        return MessageDigest.isEqual(expectedDigest, sha256(provided));
    }

    /** 宁可多要口令:两种路径任一看起来是管理路径就按管理路径处理。 */
    static boolean isAdminPath(HttpServletRequest request) {
        String servletPath = (request.getServletPath() == null ? "" : request.getServletPath())
                + (request.getPathInfo() == null ? "" : request.getPathInfo());
        return looksLikeAdmin(servletPath) || looksLikeAdmin(request.getRequestURI());
    }

    static boolean looksLikeAdmin(String path) {
        if (path == null || path.isEmpty()) {
            return false;
        }
        String normalized = path.replaceAll(";[^/]*", "")   // 去掉路径参数(;jsessionid=…、;x=1)
                .replaceAll("/{2,}", "/")                   // 合并连续斜杠
                .toLowerCase(Locale.ROOT);
        return normalized.equals(ADMIN_PREFIX) || normalized.startsWith(ADMIN_PREFIX + "/");
    }

    private static byte[] sha256(String text) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(text.getBytes(StandardCharsets.UTF_8));
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is required by the JDK", e);
        }
    }
}
