package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RestController;

/** 自有随机H2库+真实loopback HTTP；只证明mock契约，不是TiDB或真实支付渠道证据。 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1", "server.servlet.context-path=/mall", "spring.mvc.servlet.path=/rpc",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.mock-payment-enabled=true",
    "commerce.gateway-secret=mock-http-gateway-test-only-key-00001",
    "commerce.payment-secret=mock-http-payment-test-only-key-00002",
    "commerce.delivery-secret=mock-http-delivery-test-only-key-0003"
})
@Import({MockPaymentHttpTest.ForwardConfiguration.class, H2CommerceTestConfiguration.class})
@DirtiesContext(classMode = DirtiesContext.ClassMode.AFTER_CLASS)
class MockPaymentHttpTest {
    private static final String PREFIX = "/mall/rpc";
    private static final String DATABASE = "mock_http_" + UUID.randomUUID().toString().replace("-", "");
    @DynamicPropertySource static void database(DynamicPropertyRegistry registry) {
        registry.add("spring.datasource.url", () -> "jdbc:h2:mem:" + DATABASE + ";MODE=MySQL;LOCK_TIMEOUT=5000");
    }
    @LocalServerPort int port;
    @Autowired JdbcTemplate jdbc;
    @Autowired ObjectMapper json;
    @Autowired CommerceProperties props;

    @Test void 双前缀payment鉴权_严格正文与幂等_退款恢复失败仍返回完整摘要() throws Exception {
        var http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).followRedirects(HttpClient.Redirect.NEVER).build();
        try {
            jdbc.update("INSERT INTO pms_sku_stock(id,product_id,sku_code,price_minor,currency,stock,lock_stock,published) VALUES(1,1,'mock-http-sku',125,'CNY',10,0,1)");
            jdbc.update("INSERT INTO commerce_sku_grant(sku_id,item_config_id,count_per_unit,title) VALUES(1,10,1,'玄铁')");
            var created = call(http, "/api/orders", "gateway", "1", bytes(Map.of("requestId", "mock-http-order-001", "skuId", 1, "quantity", 2, "buyerAccount", "测试账号", "buyerOwnerRevision", "1")));
            assertEquals(200, created.statusCode(), created.body());
            String orderId = json.readTree(created.body()).path("orderId").asText();
            byte[] input = bytes(Map.of("orderId", orderId, "requestId", "mock-http-payment-001"));
            for (String role : List.of("gateway", "delivery")) {
                assertEquals(401, call(http, "/internal/mock/pay", role, role.equals("gateway") ? "1" : "trade", input).statusCode());
                assertEquals(401, call(http, "/internal/mock/reconcile", role, role.equals("gateway") ? "1" : "trade", utf8("{}")).statusCode());
            }
            assertEquals(401, call(http, "/internal/mock/pay", "payment", "unknown-adapter", input).statusCode());
            assertEquals(401, call(http, "/api/forward-mock", "gateway", "1", input).statusCode());
            assertEquals(0, count("commerce_mock_transaction"));
            for (String invalid : List.of(
                "null", "[]", "{}", "{\"orderId\":null,\"requestId\":\"mock-http-payment-001\"}",
                "{\"orderId\":\"" + orderId + "\",\"requestId\":3}",
                "{\"orderId\":\"" + orderId + "\",\"requestId\":\"mock-http-payment-001\",\"amountMinor\":1}",
                "{\"orderId\":\"" + orderId + "\",\"requestId\":\"mock-http-payment-001\",\"paid\":true}",
                "{\"orderId\":\"" + orderId + "\",\"requestId\":\"mock-http-payment-001\",\"currency\":\"USD\"}",
                new String(input, StandardCharsets.UTF_8) + " {}",
                "{\"orderId\":\"" + orderId + "\",\"requestId\":\"mock-http-payment-001\",\"requestId\":\"mock-http-payment-002\"}")) {
                var rejected = call(http, "/internal/mock/pay", "payment", "payment-adapter", utf8(invalid));
                assertEquals(400, rejected.statusCode(), invalid + " => " + rejected.body());
            }
            assertEquals(400, call(http, "/internal/mock/reconcile", "payment", "payment-adapter", utf8("{\"limit\":1}")).statusCode());
            assertEquals(400, call(http, "/internal/mock/reconcile", "payment", "payment-adapter", utf8("{} {} ")).statusCode());
            assertEquals(0, count("commerce_payment_receipt"));
            assertEquals(0, count("commerce_mock_transaction"));
            props.setMockPaymentEnabled(false);
            try { assertEquals(503, call(http, "/internal/mock/pay", "payment", "payment-adapter", input).statusCode()); }
            finally { props.setMockPaymentEnabled(true); }

            var signed = request("/internal/mock/pay", "payment", "payment-adapter", input);
            var paid = http.send(signed, HttpResponse.BodyHandlers.ofString());
            assertEquals(200, paid.statusCode(), paid.body());
            JsonNode receipt = json.readTree(paid.body());
            assertEquals(orderId, receipt.path("orderId").asText());
            assertEquals(250, receipt.path("amountMinor").asLong());
            assertEquals("CNY", receipt.path("currency").asText());
            assertEquals("DELIVERY_PENDING", receipt.path("state").asText());
            assertEquals(409, http.send(signed, HttpResponse.BodyHandlers.ofString()).statusCode());
            assertEquals(receipt, json.readTree(call(http, "/internal/mock/pay", "payment", "payment-adapter", input).body()));
            assertEquals(409, call(http, "/internal/mock/pay", "payment", "payment-adapter",
                bytes(Map.of("orderId", orderId, "requestId", "mock-http-payment-002"))).statusCode());
            assertEquals(1, count("commerce_mock_transaction"));
            assertEquals(1, count("commerce_payment_receipt"));
            assertEquals(8, jdbc.queryForObject("SELECT stock FROM pms_sku_stock WHERE id=1", Integer.class));
            assertEquals(0, jdbc.queryForObject("SELECT lock_stock FROM pms_sku_stock WHERE id=1", Integer.class));

            byte[] delivery = bytes(new DeliveryApi.Receipt(orderId, "123", "REFUND_PENDING", "a".repeat(64)));
            assertEquals(200, call(http, "/internal/deliveries/receipt", "delivery", "trade", delivery).statusCode());
            // 一个损坏的未投递付款原件与一个可恢复退款并存，失败不能吞掉后项或摘要。
            var another = call(http, "/api/orders", "gateway", "2", bytes(Map.of("requestId", "mock-http-order-002", "skuId", 1, "quantity", 1, "buyerAccount", "测试账号", "buyerOwnerRevision", "1")));
            assertEquals(200, another.statusCode(), another.body());
            String badId = json.readTree(another.body()).path("orderId").asText();
            assertEquals(200, call(http, "/internal/mock/pay", "payment", "payment-adapter",
                bytes(Map.of("orderId", badId, "requestId", "mock-http-bad-payment"))).statusCode());
            assertEquals(1, jdbc.update("UPDATE commerce_mock_transaction SET request_id=?,applied=0 WHERE kind='PAYMENT' AND order_sn=?", utf8("bad!"), badId));
            var summary = call(http, "/internal/mock/reconcile", "payment", "payment-adapter", utf8("{}"));
            assertEquals(503, summary.statusCode(), summary.body());
            JsonNode result = json.readTree(summary.body());
            assertEquals(2, result.path("attempted").asInt());
            assertEquals(1, result.path("succeeded").asInt());
            assertEquals(1, result.path("failed").asInt());
            assertEquals(0, result.path("scanFailures").asInt());
            assertFalse(result.path("busy").asBoolean());
            assertEquals("REFUNDED", jdbc.queryForObject("SELECT status FROM commerce_refund_intent WHERE order_sn=?", String.class, orderId));
            assertEquals(1, count("commerce_refund_receipt"));
            assertEquals(3, count("commerce_mock_transaction"));
            var repeated = json.readTree(call(http, "/internal/mock/reconcile", "payment", "payment-adapter", utf8("{}")).body());
            assertEquals(1, repeated.path("failed").asInt());
            assertEquals(0, repeated.path("succeeded").asInt());
            assertEquals(1, count("commerce_refund_receipt"));
            assertEquals(404, call(http, "/internal/mock/unknown", "payment", "payment-adapter", utf8("{}")).statusCode());
        } finally {
            http.shutdownNow();
            assertTrue(http.awaitTermination(Duration.ofSeconds(5)), "本例HTTP客户端清理超时");
        }
    }

    private byte[] bytes(Object value) throws Exception { return json.writeValueAsBytes(value); }
    private static byte[] utf8(String value) { return value.getBytes(StandardCharsets.UTF_8); }
    private int count(String table) { return jdbc.queryForObject("SELECT COUNT(*) FROM " + table, Integer.class); }
    private HttpResponse<String> call(HttpClient http, String route, String role, String subject, byte[] input) throws Exception {
        return http.send(request(route, role, subject, input), HttpResponse.BodyHandlers.ofString());
    }
    private HttpRequest request(String route, String role, String subject, byte[] body) {
        String path = PREFIX + route;
        String timestamp = Long.toString(System.currentTimeMillis()); String nonce = UUID.randomUUID().toString();
        String secret = switch (role) {
            case "payment" -> "mock-http-payment-test-only-key-00002";
            case "delivery" -> "mock-http-delivery-test-only-key-0003";
            default -> "mock-http-gateway-test-only-key-00001";
        };
        return HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + path)).timeout(Duration.ofSeconds(20))
            .header("Content-Type", "application/json").header("X-Commerce-Subject", subject)
            .header("X-Commerce-Timestamp", timestamp).header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature", HmacAuthentication.sign(secret, role, subject, timestamp, nonce, "POST", path, body))
            .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
    }
    @TestConfiguration(proxyBeanMethods = false)
    static class ForwardConfiguration {
        @Bean ForwardController mockForwardController() { return new ForwardController(); }
    }
    @RestController
    @org.springframework.boot.test.context.TestComponent
    static class ForwardController {
        @PostMapping("/api/forward-mock")
        void forward(HttpServletRequest request, HttpServletResponse response) throws ServletException, IOException {
            request.getRequestDispatcher("/rpc/internal/mock/pay").forward(request, response);
        }
    }
}
