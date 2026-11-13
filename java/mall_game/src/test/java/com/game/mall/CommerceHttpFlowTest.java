package com.game.mall;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Import;
import org.springframework.jdbc.core.JdbcTemplate;

/** 通过真实 HTTP 与实际 Spring 装配验收订单切片；H2 不能代替 TiDB 集成证据。 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
        "server.address=127.0.0.1",
        "spring.datasource.url=jdbc:h2:mem:commerce_http_flow;MODE=MySQL;DB_CLOSE_DELAY=-1;LOCK_TIMEOUT=5000",
        "spring.datasource.username=sa",
        "spring.datasource.password=",
        "spring.sql.init.mode=always",
        "spring.sql.init.schema-locations=classpath:db/schema.sql",
        "commerce.require-tidb=false",
        "commerce.run-mode=test",
        "commerce.allowed-payment-channels=test-channel",
        "commerce.gateway-secret=http-flow-gateway-test-only-key-0001",
        "commerce.payment-secret=http-flow-payment-test-only-key-0002",
        "commerce.delivery-secret=http-flow-delivery-test-only-key-0003"
})
@Import(H2CommerceTestConfiguration.class)
class CommerceHttpFlowTest {
    private static final String BUYER = "18446744073709551615";
    private static final String GATEWAY_KEY = "http-flow-gateway-test-only-key-0001";
    private static final String PAYMENT_KEY = "http-flow-payment-test-only-key-0002";
    private static final String DELIVERY_KEY = "http-flow-delivery-test-only-key-0003";

    @LocalServerPort int port;
    @Value("${server.servlet.context-path:}") String contextPath;
    @Value("${spring.mvc.servlet.path:}") String servletPath;
    @Autowired JdbcTemplate jdbc;
    @Autowired ObjectMapper json;
    private final HttpClient client = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    @Test
    void signedOrdersReserveOnceAuthorizeOwnershipAndStopAtPendingDelivery() throws Exception {
        jdbc.update("INSERT INTO pms_sku_stock(id,product_id,sku_code,price_minor,currency,stock,lock_stock,published) VALUES (701,71,'http-sku',1250,'CNY',4,0,1)");
        jdbc.update("INSERT INTO commerce_sku_grant(sku_id,item_config_id,count_per_unit,title) VALUES(701,10,1,'玄铁')");
        byte[] creation = json.writeValueAsBytes(Map.of("requestId", "http-buyer-request-001", "skuId", 701, "quantity", 2,
            "buyerAccount", "测试账号", "buyerOwnerRevision", "1"));
        for (String invalid : java.util.List.of(
                "{\"requestId\":\"missing-owner-001\",\"skuId\":701,\"quantity\":1}",
                "{\"requestId\":\"numeric-owner-001\",\"skuId\":701,\"quantity\":1,\"buyerAccount\":\"测试账号\",\"buyerOwnerRevision\":1}",
                "{\"requestId\":\"duplicate-owner-001\",\"skuId\":701,\"quantity\":1,\"buyerAccount\":\"测试账号\",\"buyerOwnerRevision\":\"1\",\"buyerOwnerRevision\":\"2\"}")) {
            assertEquals(400, call("POST", "/api/orders", "gateway", BUYER, invalid.getBytes(StandardCharsets.UTF_8)).statusCode());
        }
        HttpRequest original = signed("POST", "/api/orders", "gateway", BUYER, creation);
        HttpResponse<String> created = send(original);
        assertEquals(200, created.statusCode(), created.body());
        JsonNode order = json.readTree(created.body());
        String orderId = order.get("orderId").asText();
        assertEquals(BUYER, order.get("buyerId").asText());
        assertEquals(2500, order.get("amountMinor").asLong());
        assertEquals("AWAITING_PAYMENT", order.get("state").asText());
        assertTrue(order.get("paymentDeadlineMs").asLong() > System.currentTimeMillis());
        byte[] ownedQuery = json.writeValueAsBytes(Map.of("orderId", orderId, "buyerAccount", "测试账号", "buyerOwnerRevision", "1"));
        assertEquals(order, json.readTree(call("POST", "/api/orders/query", "gateway", BUYER, ownedQuery).body()));
        var ownedPage = json.readTree(call("POST", "/api/orders/list", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("afterOrderId", "", "limit", 20, "buyerAccount", "测试账号", "buyerOwnerRevision", "1"))).body());
        assertEquals(1, ownedPage.size()); assertEquals(order, ownedPage.get(0));
        assertEquals(404, call("GET", "/api/orders/" + orderId, "gateway", BUYER, new byte[0]).statusCode());
        assertEquals(404, call("POST", "/api/orders/query", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("orderId", orderId, "buyerAccount", "过户新账号", "buyerOwnerRevision", "2"))).statusCode());
        assertEquals(404, call("POST", "/api/orders/query", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("orderId", orderId, "buyerAccount", "测试账号", "buyerOwnerRevision", "3"))).statusCode());
        assertEquals(404, call("POST", "/api/orders/cancel", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("orderId", orderId, "buyerAccount", "测试账号", "buyerOwnerRevision", "3"))).statusCode());
        assertTrue(json.readTree(call("POST", "/api/orders/list", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("afterOrderId", "", "limit", 20, "buyerAccount", "测试账号", "buyerOwnerRevision", "3"))).body()).isEmpty());
        assertEquals(400, call("POST", "/api/orders/query", "gateway", BUYER,
            json.writeValueAsBytes(Map.of("orderId", orderId, "buyerAccount", "测试账号", "buyerOwnerRevision", 1))).statusCode());

        // payment 查询同一冻结订单，不要求伪造买家身份；gateway/delivery 不能借查询入口跨角色读取。
        byte[] query = json.writeValueAsBytes(Map.of("orderId", orderId));
        HttpRequest paymentView = signed("POST", "/internal/payments/order-view", "payment", "payment-adapter", query);
        HttpResponse<String> queried = send(paymentView);
        assertEquals(200, queried.statusCode(), queried.body());
        assertEquals(order, json.readTree(queried.body()));
        assertEquals(409, send(paymentView).statusCode(), "只读查询仍消耗原认证 nonce");
        assertEquals(401, call("POST", "/internal/payments/order-view", "gateway", BUYER, query).statusCode());
        assertEquals(401, call("POST", "/internal/payments/order-view", "delivery", "trade", query).statusCode());
        assertEquals(401, call("POST", "/internal/payments/order-view", "payment", "unknown-adapter", query).statusCode());
        assertEquals(400, call("POST", "/internal/payments/order-view", "payment", "payment-adapter",
            json.writeValueAsBytes(Map.of("orderId", "NOT-AN-ORDER"))).statusCode());
        assertEquals(404, call("POST", "/internal/payments/order-view", "payment", "payment-adapter",
            json.writeValueAsBytes(Map.of("orderId", "00000000-0000-0000-0000-000000000001"))).statusCode());
        byte[] skuQuery = json.writeValueAsBytes(Map.of("afterSkuId", 0, "limit", 20));
        HttpResponse<String> skus = call("POST", "/internal/payments/skus", "payment", "payment-adapter", skuQuery);
        assertEquals(200, skus.statusCode(), skus.body());
        JsonNode skuPage = json.readTree(skus.body()).path("skus");
        assertEquals(1, skuPage.size());
        assertEquals(701, skuPage.get(0).path("skuId").asLong());
        assertEquals(1250, skuPage.get(0).path("priceMinor").asLong());
        assertEquals(2, skuPage.get(0).path("available").asLong());
        assertEquals(401, call("POST", "/internal/payments/skus", "gateway", BUYER, skuQuery).statusCode());
        assertEquals(401, call("POST", "/internal/payments/skus", "delivery", "trade", skuQuery).statusCode());
        assertEquals(400, call("POST", "/internal/payments/skus", "payment", "payment-adapter",
            json.writeValueAsBytes(Map.of("afterSkuId", 0, "limit", 101))).statusCode());
        assertEquals(404, call("POST", "/internal/payments/skus/unknown", "payment", "payment-adapter", skuQuery).statusCode());
        assertEquals(1, count("oms_order"));
        assertEquals(0, count("commerce_payment_receipt"));
        assertEquals(0, count("commerce_delivery_intent"));
        assertEquals(0, count("commerce_refund_intent"));

        // 原签名不能重放；以新 nonce 重试同业务请求号只返回原订单。
        assertEquals(409, send(original).statusCode());
        HttpResponse<String> replay = call("POST", "/api/orders", "gateway", BUYER, creation);
        assertEquals(200, replay.statusCode(), replay.body());
        assertEquals(orderId, json.readTree(replay.body()).get("orderId").asText());
        assertEquals(1, count("oms_order"));
        assertEquals(2, stockValue("lock_stock"));
        assertEquals(4, stockValue("stock"));
        byte[] changed = json.writeValueAsBytes(Map.of("requestId", "http-buyer-request-001", "skuId", 701, "quantity", 1,
            "buyerAccount", "测试账号", "buyerOwnerRevision", "1"));
        assertEquals(409, call("POST", "/api/orders", "gateway", BUYER, changed).statusCode());
        assertEquals(404, call("GET", "/api/orders/" + orderId, "gateway", "2", new byte[0]).statusCode());
        assertEquals(401, send(HttpRequest.newBuilder(uri("/api/orders/" + orderId)).GET().build()).statusCode());

        byte[] payment = json.writeValueAsBytes(Map.of("orderId", orderId, "channel", "test-channel", "transactionId", "HttpPayment001", "amountMinor", 2500, "currency", "CNY"));
        assertEquals(401, call("POST", "/internal/payments", "gateway", BUYER, payment).statusCode());
        HttpResponse<String> paid = call("POST", "/internal/payments", "payment", "payment-adapter", payment);
        assertEquals(200, paid.statusCode(), paid.body());
        JsonNode paidOutcome = json.readTree(paid.body());
        assertEquals("DELIVERY_PENDING", paidOutcome.path("order").path("state").asText());
        assertFalse(paidOutcome.hasNonNull("refundReason"), "订单本身那笔不退款");
        assertEquals(paidOutcome.path("order"), json.readTree(
            call("POST", "/internal/payments/order-view", "payment", "payment-adapter", query).body()));
        HttpResponse<String> replayed = call("POST", "/internal/payments", "payment", "payment-adapter", payment);
        assertEquals(200, replayed.statusCode());
        assertEquals(paidOutcome, json.readTree(replayed.body()), "重放同一笔回同一结论");
        assertEquals(1, count("commerce_payment_receipt"));
        assertEquals(1, count("commerce_delivery_intent"));
        assertEquals(2, stockValue("stock"));
        assertEquals(0, stockValue("lock_stock"));

        // 金额与订单不符的一笔:同样回 200 并落库,订单视图不变,适配器从 refundReason 读出这笔要退、为什么退。
        byte[] mismatched = json.writeValueAsBytes(Map.of("orderId", orderId, "channel", "test-channel", "transactionId", "HttpMismatch001", "amountMinor", 2499, "currency", "CNY"));
        HttpResponse<String> abnormal = call("POST", "/internal/payments", "payment", "payment-adapter", mismatched);
        assertEquals(200, abnormal.statusCode(), abnormal.body());
        assertEquals("AMOUNT_MISMATCH", json.readTree(abnormal.body()).path("refundReason").asText());
        assertEquals(paidOutcome.path("order"), json.readTree(abnormal.body()).path("order"));
        assertEquals(2, count("commerce_payment_receipt"));
        assertEquals(1, count("commerce_refund_intent"));
        assertEquals(1, count("commerce_delivery_intent"));
        assertEquals(2, stockValue("stock"));

        // 已付订单又收到一笔金额币种都对的付款(重复付款,2026-11-12 补 HTTP 层用例):与金额不符同形,回 200、落库,
        // refundReason 为 DUPLICATE_PAYMENT,重放回同一结论。订单本身那笔的重放仍不带退款原因 —— 适配器只靠 refundReason
        // 分清「重复付款」与「正常回放」,两者的 HTTP 状态与订单视图完全相同。
        byte[] duplicate = json.writeValueAsBytes(Map.of("orderId", orderId, "channel", "test-channel", "transactionId", "HttpDuplicate001", "amountMinor", 2500, "currency", "CNY"));
        HttpResponse<String> duplicated = call("POST", "/internal/payments", "payment", "payment-adapter", duplicate);
        assertEquals(200, duplicated.statusCode(), duplicated.body());
        JsonNode duplicateOutcome = json.readTree(duplicated.body());
        assertEquals("DUPLICATE_PAYMENT", duplicateOutcome.path("refundReason").asText());
        assertEquals(paidOutcome.path("order"), duplicateOutcome.path("order"), "重复付款不绑定订单,订单视图不变");
        HttpResponse<String> duplicateReplay = call("POST", "/internal/payments", "payment", "payment-adapter", duplicate);
        assertEquals(200, duplicateReplay.statusCode(), duplicateReplay.body());
        assertEquals(duplicateOutcome, json.readTree(duplicateReplay.body()), "重复付款重放回同一结论,仍带 DUPLICATE_PAYMENT");
        HttpResponse<String> ownReplay = call("POST", "/internal/payments", "payment", "payment-adapter", payment);
        assertEquals(200, ownReplay.statusCode(), ownReplay.body());
        assertEquals(paidOutcome, json.readTree(ownReplay.body()), "订单本身那笔的重放仍不带退款原因");
        assertEquals(3, count("commerce_payment_receipt"));
        assertEquals(2, count("commerce_refund_intent"), "重复付款排进退款,重放不再加一条");
        assertEquals(1, count("commerce_delivery_intent"), "重复付款不发第二份交付");
        assertEquals(2, stockValue("stock"), "重复付款不扣第二次库存");
        assertEquals(404, call("POST", "/api/orders/" + orderId + "/cancel", "gateway", BUYER, new byte[0]).statusCode());
        assertEquals(409, call("POST", "/api/orders/cancel", "gateway", BUYER, ownedQuery).statusCode());

        byte[] second = json.writeValueAsBytes(Map.of("requestId", "http-buyer-request-002", "skuId", 701, "quantity", 1,
            "buyerAccount", "测试账号", "buyerOwnerRevision", "1"));
        HttpResponse<String> secondCreated = call("POST", "/api/orders", "gateway", BUYER, second);
        assertEquals(200, secondCreated.statusCode(), secondCreated.body());
        String secondId = json.readTree(secondCreated.body()).get("orderId").asText();
        assertEquals(1, stockValue("lock_stock"));
        byte[] secondIdentity = json.writeValueAsBytes(Map.of("orderId", secondId, "buyerAccount", "测试账号", "buyerOwnerRevision", "1"));
        HttpResponse<String> cancelled = call("POST", "/api/orders/cancel", "gateway", BUYER, secondIdentity);
        assertEquals(200, cancelled.statusCode(), cancelled.body());
        assertEquals("CANCELLED", json.readTree(cancelled.body()).get("state").asText());
        assertEquals(json.readTree(cancelled.body()), json.readTree(call("POST", "/internal/payments/order-view",
            "payment", "payment-adapter", json.writeValueAsBytes(Map.of("orderId", secondId))).body()));
        assertEquals(200, call("POST", "/api/orders/cancel", "gateway", BUYER, secondIdentity).statusCode());
        assertEquals(2, stockValue("stock"));
        assertEquals(0, stockValue("lock_stock"));
        assertEquals(1, count("commerce_delivery_intent"));
    }

    private HttpResponse<String> call(String method, String path, String role, String subject, byte[] body) throws Exception {
        return send(signed(method, path, role, subject, body));
    }

    private HttpRequest signed(String method, String path, String role, String subject, byte[] body) {
        String timestamp = Long.toString(System.currentTimeMillis());
        String nonce = UUID.randomUUID().toString();
        String key = role.equals("payment") ? PAYMENT_KEY : role.equals("delivery") ? DELIVERY_KEY : GATEWAY_KEY;
        return HttpRequest.newBuilder(uri(path)).timeout(Duration.ofSeconds(8))
                .header("Content-Type", "application/json")
                .header("X-Commerce-Subject", subject)
                .header("X-Commerce-Timestamp", timestamp)
                .header("X-Commerce-Nonce", nonce)
                .header("X-Commerce-Signature", HmacAuthentication.sign(key, role, subject, timestamp, nonce, method, requestPath(path), body))
                .method(method, HttpRequest.BodyPublishers.ofByteArray(body)).build();
    }

    private HttpResponse<String> send(HttpRequest request) throws Exception {
        return client.send(request, HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
    }

    private URI uri(String path) { return URI.create("http://127.0.0.1:" + port + requestPath(path)); }
    private String requestPath(String path) { return contextPath + ("/".equals(servletPath) ? "" : servletPath) + path; }
    private int count(String table) { return jdbc.queryForObject("SELECT COUNT(*) FROM " + table, Integer.class); }
    private int stockValue(String column) { return jdbc.queryForObject("SELECT " + column + " FROM pms_sku_stock WHERE id=701", Integer.class); }
}
