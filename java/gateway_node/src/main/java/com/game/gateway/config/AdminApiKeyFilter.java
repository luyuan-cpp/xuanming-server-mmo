package com.game.gateway.config;

import jakarta.servlet.*;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.Set;
import java.util.regex.Pattern;

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
 *
 * <p>2026-11-26 收口(docs/design/admin-identity-and-dual-control.md §8 批 6):人只面对后台(java/gm_admin),
 * admin.api-key 只挂给后台工作负载,后台从登录会话填 X-Admin-Operator。
 * <ul>
 *   <li>写操作(POST / PUT / PATCH / DELETE)必须带且只带一个合规的 X-Admin-Operator(与身份提供方主体同一字符集),否则 400;
 *       读操作不要求。</li>
 *   <li>配置表发布与回滚是机器调用(go/table_sync/cmd/table-bundle),用另一把口令 admin.machine-key:它只能访问 /admin/tables/**,
 *       操作人必须是 table-bundle;人用的口令不能以 table-bundle 的名义写。没配就不开这条路。两把口令相同则拒绝启动。</li>
 *   <li>每一次写操作在响应之后打一行 AUDIT(方法、路径、状态、操作人、口令种类、来源地址),所有管理控制器都覆盖到,新加的也不会漏;
 *       热关停与配置表两个控制器另有带业务细节的 AUDIT 行。</li>
 * </ul>
 */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
public class AdminApiKeyFilter implements Filter {

    private static final Logger log = LoggerFactory.getLogger(AdminApiKeyFilter.class);
    private static final String HEADER = "X-Admin-Key";
    static final String OPERATOR_HEADER = "X-Admin-Operator";
    /** 机器口令的唯一操作人;人用的口令不能用这个名字。 */
    static final String MACHINE_OPERATOR = "table-bundle";
    private static final String ADMIN_PREFIX = "/admin";
    private static final String MACHINE_PREFIX = "/admin/tables";
    private static final Pattern OPERATOR = Pattern.compile("[A-Za-z0-9._@-]{1,64}");
    private static final Set<String> WRITE_METHODS = Set.of("POST", "PUT", "PATCH", "DELETE");

    private final byte[] expectedDigest;
    private final byte[] machineDigest;

    public AdminApiKeyFilter(String apiKey) {
        this(apiKey, "");
    }

    @Autowired
    public AdminApiKeyFilter(@Value("${admin.api-key}") String apiKey, @Value("${admin.machine-key:}") String machineKey) {
        this.expectedDigest = apiKey == null || apiKey.isBlank() ? null : sha256(apiKey);
        boolean machine = machineKey != null && !machineKey.isBlank();
        if (machine && (machineKey.getBytes(StandardCharsets.UTF_8).length < 32 || machineKey.equals(apiKey))) {
            throw new IllegalStateException("admin.machine-key must be at least 32 bytes and differ from admin.api-key");
        }
        this.machineDigest = machine ? sha256(machineKey) : null;
    }

    @Override
    public void doFilter(ServletRequest request, ServletResponse response, FilterChain chain)
            throws IOException, ServletException {

        HttpServletRequest httpReq = (HttpServletRequest) request;
        if (!isAdminPath(httpReq)) {
            chain.doFilter(request, response);
            return;
        }

        HttpServletResponse httpResp = (HttpServletResponse) response;
        String provided = httpReq.getHeader(HEADER);
        boolean human = matches(expectedDigest, provided);
        boolean machine = !human && matches(machineDigest, provided);
        if (!human && !machine) {
            reject(httpResp, HttpServletResponse.SC_UNAUTHORIZED, "Unauthorized");
            return;
        }
        if (machine && !isUnder(httpReq, MACHINE_PREFIX)) {
            reject(httpResp, HttpServletResponse.SC_FORBIDDEN, "machine key is limited to /admin/tables");
            return;
        }
        boolean write = WRITE_METHODS.contains(httpReq.getMethod().toUpperCase(Locale.ROOT));
        String operator = operator(httpReq);
        // 机器口令只能以 table-bundle 写;人用的口令不能冒用 table-bundle。
        if (write && (operator == null || machine != MACHINE_OPERATOR.equals(operator))) {
            reject(httpResp, HttpServletResponse.SC_BAD_REQUEST,
                    machine ? "machine key requests must send X-Admin-Operator: table-bundle" : "write requests need one valid X-Admin-Operator");
            return;
        }
        chain.doFilter(request, response);
        if (write) {
            log.warn("AUDIT gateway admin write method={} path={} status={} operator={} key={} remote={}", httpReq.getMethod(),
                    normalize((httpReq.getServletPath() == null ? "" : httpReq.getServletPath())
                            + (httpReq.getPathInfo() == null ? "" : httpReq.getPathInfo())),
                    httpResp.getStatus(), operator, machine ? "machine" : "admin", request.getRemoteAddr());
        }
    }

    boolean authorized(String provided) {
        return matches(expectedDigest, provided) || matches(machineDigest, provided);
    }

    private static boolean matches(byte[] digest, String provided) {
        return digest != null && provided != null && MessageDigest.isEqual(digest, sha256(provided));
    }

    /** 只有一个且合规的 X-Admin-Operator 才算数;没有、重复或不合规都返回 null。 */
    static String operator(HttpServletRequest request) {
        List<String> values = Collections.list(request.getHeaders(OPERATOR_HEADER));
        if (values.size() != 1 || values.get(0) == null) {
            return null;
        }
        String value = values.get(0);
        return OPERATOR.matcher(value).matches() ? value : null;
    }

    private static void reject(HttpServletResponse response, int status, String error) throws IOException {
        response.setStatus(status);
        response.setContentType("application/json");
        response.getWriter().write("{\"error\":\"" + error + "\"}");
    }

    /** 容器规范化后的路径与原始 URI 都必须落在 prefix 之下,且不带 ..(机器口令按最严格的一种判断,与 isAdminPath 的「宁可多要」相反)。 */
    static boolean isUnder(HttpServletRequest request, String prefix) {
        String servletPath = (request.getServletPath() == null ? "" : request.getServletPath())
                + (request.getPathInfo() == null ? "" : request.getPathInfo());
        for (String path : List.of(normalize(servletPath), normalize(request.getRequestURI() == null ? "" : request.getRequestURI()))) {
            if (!(path.equals(prefix) || path.startsWith(prefix + "/")) || path.contains("/..") || path.contains("%")) {
                return false;
            }
        }
        return true;
    }

    private static String normalize(String path) {
        return path.replaceAll(";[^/]*", "").replaceAll("/{2,}", "/").toLowerCase(Locale.ROOT);
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
        String normalized = normalize(path); // 去掉路径参数(;jsessionid=…、;x=1)、合并连续斜杠、转小写
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
