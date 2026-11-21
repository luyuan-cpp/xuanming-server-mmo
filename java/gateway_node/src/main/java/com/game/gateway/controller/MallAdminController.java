package com.game.gateway.controller;

import com.game.gateway.service.MallAdminClient;
import jakarta.servlet.http.HttpServletRequest;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Collections;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.core.io.ClassPathResource;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.server.ResponseStatusException;

/** 复用网关运维令牌；方法内再次鉴权，context/servlet前缀或forward不能绕过。 */
@RestController
public final class MallAdminController {
    private final MallAdminClient client;
    private final byte[] adminKey;
    public MallAdminController(MallAdminClient client, @Value("${admin.api-key:}") String adminKey) {
        this.client = client;
        this.adminKey = adminKey == null ? new byte[0] : adminKey.getBytes(StandardCharsets.UTF_8);
    }
    @GetMapping(value = "/mall-admin", produces = MediaType.TEXT_HTML_VALUE)
    public ResponseEntity<byte[]> page() throws IOException {
        var bytes = new ClassPathResource("mall-admin.html").getContentAsByteArray();
        return ResponseEntity.ok().header("Cache-Control", "no-store")
            .header("X-Content-Type-Options", "nosniff")
            .header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
            .contentType(MediaType.TEXT_HTML).body(bytes);
    }
    @GetMapping(value = "/mall-admin.js", produces = "text/javascript")
    public ResponseEntity<byte[]> script() throws IOException { return asset("mall-admin.js", "text/javascript"); }
    @GetMapping(value = "/mall-admin.css", produces = "text/css")
    public ResponseEntity<byte[]> style() throws IOException { return asset("mall-admin.css", "text/css"); }

    private static ResponseEntity<byte[]> asset(String name, String contentType) throws IOException {
        return ResponseEntity.ok().header("Cache-Control", "no-store").header("X-Content-Type-Options", "nosniff")
            .contentType(MediaType.parseMediaType(contentType)).body(new ClassPathResource(name).getContentAsByteArray());
    }
    @PostMapping({
        "/admin/mall/skus/list", "/admin/mall/skus/create", "/admin/mall/skus/update", "/admin/mall/skus/stock",
        "/admin/mall/order-view",
        "/admin/mall/manual/list", "/admin/mall/manual/orders/resolve", "/admin/mall/manual/refunds/resolve",
        "/admin/mall/manual/consignments/resolve"
    })
    public ResponseEntity<byte[]> execute(HttpServletRequest request) throws IOException {
        if (adminKey.length < 32) { throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "请先配置至少32字节的运维令牌"); }
        var keys = Collections.list(request.getHeaders("X-Admin-Key"));
        if (keys.size() != 1 || keys.getFirst().length() > 256
            || !MessageDigest.isEqual(adminKey, keys.getFirst().getBytes(StandardCharsets.UTF_8))) {
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "运维认证失败");
        }
        String route = request.getPathInfo() == null ? request.getServletPath() : request.getPathInfo();
        String prefix = "/admin/mall/";
        if (route == null || !route.startsWith(prefix) || request.getQueryString() != null) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "管理路径非法");
        }
        byte[] body = request.getInputStream().readNBytes(16_385);
        if (body.length > 16_384) { throw new ResponseStatusException(HttpStatus.PAYLOAD_TOO_LARGE, "管理请求体过大"); }
        var reply = client.execute(route.substring(prefix.length()), body);
        return ResponseEntity.status(reply.status()).header("Cache-Control", "no-store")
            .contentType(MediaType.APPLICATION_JSON).body(reply.body());
    }
}
