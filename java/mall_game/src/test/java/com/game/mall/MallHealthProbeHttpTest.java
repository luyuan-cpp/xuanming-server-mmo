package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import org.junit.jupiter.api.Test;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Import;

/**
 * 商城 HTTP 健康探针契约:只有 /actuator/health/readiness 与 /actuator/health/liveness 的无正文 GET 免 HMAC,
 * 复合 /actuator/health 与其余路径仍按 fail-closed 拦截。
 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1",
    "spring.datasource.url=jdbc:h2:mem:mall_health_probe_http;MODE=MySQL;DB_CLOSE_DELAY=-1;LOCK_TIMEOUT=5000",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.allowed-payment-channels=test",
    "commerce.gateway-secret=health-gateway-test-only-key-000001",
    "commerce.payment-secret=health-payment-test-only-key-000002",
    "commerce.delivery-secret=health-delivery-test-only-key-00003",
    "commerce.admin-secret=health-admin-test-only-key-0000004",
    "management.otlp.tracing.export.enabled=false"
})
@Import(H2CommerceTestConfiguration.class)
class MallHealthProbeHttpTest {
    @LocalServerPort int port;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    private HttpResponse<String> get(String path) throws Exception {
        return http.send(HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + path))
            .timeout(Duration.ofSeconds(5)).GET().build(), HttpResponse.BodyHandlers.ofString());
    }

    @Test void 探针分组免鉴权返回UP_其余端点继续拦截() throws Exception {
        for (String probe : new String[]{"/actuator/health/readiness", "/actuator/health/liveness"}) {
            HttpResponse<String> resp = get(probe);
            assertEquals(200, resp.statusCode(), probe + " body=" + resp.body());
            assertTrue(resp.body().contains("\"UP\""), probe + " body=" + resp.body());
        }
        // 复合 /actuator/health 含 DB 指示器,不放开免鉴权;其余 actuator 端点同样拦截
        assertEquals(401, get("/actuator/health").statusCode());
        assertEquals(401, get("/actuator/info").statusCode());
        assertEquals(400, get("/actuator/health/readiness?foo=1").statusCode());
        HttpResponse<String> post = http.send(
            HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + "/actuator/health/readiness"))
                .timeout(Duration.ofSeconds(5)).POST(HttpRequest.BodyPublishers.ofString("{}")).build(),
            HttpResponse.BodyHandlers.ofString());
        assertEquals(401, post.statusCode());
    }
}
