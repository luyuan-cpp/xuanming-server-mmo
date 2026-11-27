package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.HexFormat;
import java.util.Set;
import java.util.UUID;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * 商城管理入口(java/mall_game 的 /internal/admin/*)的客户端,原来在网关(MallAdminClient),双人复核批 4 迁进后台。
 *
 * <p>调用方证明是商城的 HMAC v1:角色 admin、主体 commerce.admin-subject,签名覆盖主体、时刻、nonce、方法、原始路径与正文摘要。
 * 写命令带流水生成的批准记录(在正文里,被签名覆盖),请求号就是批准号;商城按 (主体, 请求号) 幂等,
 * 所以确定的拒绝之外的一切失败都记成「结果未知」,用原申请重试由商城判重。
 */
@Component
public class GameMallAdmin {
    /** 只读命令:直接调用,不进流水。 */
    static final Set<String> READS = Set.of("manual/list", "order-view", "skus/list");
    /** 写命令:只经运营处置流水放行。 */
    static final Set<String> WRITES = Set.of("manual/orders/resolve", "manual/refunds/resolve", "manual/consignments/resolve",
            "manual/incomes/dispute", "skus/create", "skus/update", "skus/stock");
    /** 商城对确定拒绝的回包状态;其余(503、5xx、超时、断连)都可能已经提交。 */
    private static final Set<Integer> DEFINITE_REFUSALS = Set.of(400, 401, 403, 404, 409, 413);
    private static final int MAX_RESPONSE_BYTES = 262_144;

    private final ObjectMapper mapper;
    private final URI base;
    private final String secret;
    private final String subject;
    private final HttpClient http;

    @Autowired
    public GameMallAdmin(ObjectMapper mapper, @Value("${gm.game.mall-url:}") String url,
            @Value("${gm.game.mall-admin-secret:}") String secret, @Value("${gm.game.mall-admin-subject:mall-admin}") String subject) {
        this(mapper, url, secret, subject, HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3))
                .followRedirects(HttpClient.Redirect.NEVER).build());
    }

    GameMallAdmin(ObjectMapper mapper, String url, String secret, String subject, HttpClient http) {
        this.mapper = mapper;
        this.http = http;
        this.secret = secret == null ? "" : secret.strip();
        this.subject = subject == null ? "" : subject.strip();
        if (!this.subject.matches("[a-z][a-z0-9-]{2,63}")) throw new IllegalArgumentException("gm.game.mall-admin-subject is invalid");
        this.base = url == null || url.isBlank() ? null : baseOf(url.strip());
    }

    /** 与 GmController 访问网关同一条规则:HTTPS,或本机回环 / 集群内 Service 名上的 HTTP;不带认证信息、查询串与片段。 */
    private static URI baseOf(String url) {
        URI uri;
        try { uri = URI.create(url); }
        catch (IllegalArgumentException invalid) { throw new IllegalArgumentException("gm.game.mall-url is invalid"); }
        String host = uri.getHost();
        String path = uri.getRawPath() == null ? "" : uri.getRawPath();
        // 集群内服务间本来就是明文,靠 NetworkPolicy 只放行后台与各调用方 HMAC 保护(docs/design/admin-identity-and-dual-control.md §11.4)。
        if (host == null || uri.getUserInfo() != null || uri.getQuery() != null || uri.getFragment() != null
                || path.contains("%") || path.contains(";") || path.contains("..") || path.contains("//")
                || !("https".equals(uri.getScheme())
                        || ("http".equals(uri.getScheme()) && GameServices.isLoopbackOrClusterServiceHost(host)))) {
            throw new IllegalArgumentException("gm.game.mall-url requires HTTPS, or HTTP on loopback or a cluster Service name"
                    + " (<service>.<namespace>.svc[.cluster.local]), without credentials, query or fragment");
        }
        return uri;
    }

    public boolean enabled() { return base != null && secret.getBytes(StandardCharsets.UTF_8).length >= 32; }

    /** 只读查询;任何失败都是确定的「没读到」。 */
    public ObjectNode read(String command, ObjectNode body) {
        if (!READS.contains(command)) throw new GameServices.CallFailure("INVALID_ARGUMENT", "商城只读命令不存在", false, 0);
        return post(command, body, false);
    }

    /**
     * 放行一条冻结的运营处置。frozen 是 {target, command, body, requestId};approval 为 null 表示没开双人复核(本机开发),
     * 商城要显式放开才接受。批准时刻按数字下传,与商城的 AdminApproval 记录同一形状。
     */
    public ObjectNode execute(ObjectNode frozen, ObjectNode approval) {
        String command = frozen.path("command").asText("");
        if (!WRITES.contains(command) || !(frozen.get("body") instanceof ObjectNode body))
            throw new GameServices.CallFailure("INVALID_ARGUMENT", "商城写命令不存在", false, 0);
        ObjectNode request = body.deepCopy().put("requestId", frozen.path("requestId").asText(""));
        if (approval != null) {
            request.putObject("approval").put("approvalId", approval.path("approvalId").asText())
                    .put("proposer", approval.path("proposer").asText()).put("approver", approval.path("approver").asText())
                    .put("approvedAtMs", Long.parseLong(approval.path("approvedAtMs").asText("0")));
        }
        return post(command, request, true);
    }

    private ObjectNode post(String command, ObjectNode body, boolean writing) {
        if (!enabled()) throw new GameServices.CallFailure("NOT_CONFIGURED", "商城管理通道未配置", false, 0);
        byte[] bytes = body.toString().getBytes(StandardCharsets.UTF_8);
        URI uri = URI.create(base.toString().replaceAll("/+$", "") + "/internal/admin/" + command);
        String timestamp = Long.toString(System.currentTimeMillis());
        String nonce = UUID.randomUUID().toString();
        HttpRequest request = HttpRequest.newBuilder(uri).timeout(Duration.ofSeconds(8))
                .header("Content-Type", "application/json").header("X-Commerce-Subject", subject)
                .header("X-Commerce-Timestamp", timestamp).header("X-Commerce-Nonce", nonce)
                .header("X-Commerce-Signature", signature(secret, subject, timestamp, nonce, uri.getRawPath(), bytes))
                .POST(HttpRequest.BodyPublishers.ofByteArray(bytes)).build();
        int status;
        JsonNode reply;
        try {
            HttpResponse<InputStream> response = http.send(request, HttpResponse.BodyHandlers.ofInputStream());
            status = response.statusCode();
            try (InputStream stream = response.body()) {
                byte[] content = stream.readNBytes(MAX_RESPONSE_BYTES + 1);
                if (content.length > MAX_RESPONSE_BYTES) throw new IOException("mall admin response too large");
                reply = mapper.readerFor(JsonNode.class).with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS).readValue(content);
            }
        } catch (InterruptedException interrupted) {
            Thread.currentThread().interrupt();
            throw unknownOrFailed(writing, "INTERRUPTED");
        } catch (IOException | RuntimeException unavailable) {
            throw unknownOrFailed(writing, "UNAVAILABLE");
        }
        if (status == 200 && reply instanceof ObjectNode result) return result;
        if (DEFINITE_REFUSALS.contains(status)) {
            String detail = reply == null ? "" : reply.path("error").asText("");
            throw new GameServices.CallFailure("HTTP_" + status, "商城拒绝执行：" + detail, false, 0);
        }
        throw unknownOrFailed(writing, "HTTP_" + status);
    }

    /** 写命令遇到非确定的失败都可能已经提交:记成未知,用原申请重试由商城判重。 */
    private static GameServices.CallFailure unknownOrFailed(boolean writing, String code) {
        return new GameServices.CallFailure(code, writing ? "商城处置结果未知，请用原申请重试或刷新核对" : "商城服务调用未完成：" + code, writing, 0);
    }

    /** 商城 HmacAuthentication.sign 的 v1 规范串:角色固定为 admin。 */
    static String signature(String secret, String subject, String timestamp, String nonce, String path, byte[] body) {
        try {
            String hash = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(body));
            String canonical = String.join("\n", "v1", "admin", subject, timestamp, nonce, "POST", path, hash);
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            return HexFormat.of().formatHex(mac.doFinal(canonical.getBytes(StandardCharsets.UTF_8)));
        } catch (GeneralSecurityException unavailable) {
            throw new IllegalStateException("JDK HMAC is unavailable", unavailable);
        }
    }
}
