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
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.List;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.context.annotation.Primary;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RestController;

/** 随机H2库与真实loopback HTTP；不把可信trade输入夹具声明为真实scene资产排空证明。 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1", "server.servlet.context-path=/mall", "spring.mvc.servlet.path=/rpc",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.mock-payment-enabled=true", "commerce.consignment.notice-ms=0", "commerce.consignment.cooling-ms=0",
    "commerce.gateway-secret=rmb-http-gateway-test-only-key-000001",
    "commerce.payment-secret=rmb-http-payment-test-only-key-000002",
    "commerce.delivery-secret=rmb-http-delivery-test-only-key-00003",
    "commerce.admin-secret=rmb-http-admin-test-only-key-0000004", "commerce.admin-subject=rmb-admin"
})
@Import({RmbConsignmentHttpTest.TestEndpoints.class, H2CommerceTestConfiguration.class})
@DirtiesContext(classMode = DirtiesContext.ClassMode.AFTER_CLASS)
class RmbConsignmentHttpTest {
    private static final String PREFIX = "/mall/rpc";
    private static final String DATABASE = "rmb_http_" + UUID.randomUUID().toString().replace("-", "");
    @DynamicPropertySource static void database(DynamicPropertyRegistry registry) {
        registry.add("spring.datasource.url", () -> "jdbc:h2:mem:" + DATABASE + ";MODE=MySQL;DATABASE_TO_LOWER=TRUE;LOCK_TIMEOUT=5000");
    }
    @LocalServerPort int port;
    @Autowired JdbcTemplate jdbc;
    @Autowired ObjectMapper json;

    @Test void 独立密钥与双前缀_严格正文_forward拒绝_同OMS支付交付收入出款完整回执() throws Exception {
        var http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).followRedirects(HttpClient.Redirect.NEVER).build();
        try {
            var escrow = new RmbConsignmentApi.Escrow("901", "101", "原卖家", 1, "CHARACTER", null, "1901", "a".repeat(64), 101, "角色托管");
            byte[] input = bytes(escrow);
            for (String role : List.of("gateway", "payment", "admin")) {
                assertEquals(401, call(http, "/internal/consignments/register", role, subject(role), input).statusCode());
            }
            assertEquals(401, call(http, "/internal/consignments/register", "delivery", "other-trade", input).statusCode());
            assertEquals(401, call(http, "/api/forward-consignment", "gateway", "201", input).statusCode());
            assertEquals(401, http.send(request("/internal/consignments/register", "delivery", "trade", input,
                "/internal/consignments/register"), HttpResponse.BodyHandlers.ofString()).statusCode(), "签名必须绑定完整context/servlet路径");
            String original = new String(input, StandardCharsets.UTF_8);
            for (String bad : List.of("null", "[]", "{}", original + " {}", original + "garbage",
                original.substring(0, original.length() - 1) + ",\"successful\":true}",
                original.substring(0, original.length() - 1) + ",\"listingId\":\"902\"}")) {
                var rejected = call(http, "/internal/consignments/register", "delivery", "trade", utf8(bad));
                assertEquals(400, rejected.statusCode(), bad + " => " + rejected.body());
            }
            assertEquals(0, count("commerce_consignment"));
            HttpRequest signed = request("/internal/consignments/register", "delivery", "trade", input, PREFIX + "/internal/consignments/register");
            var registered = http.send(signed, HttpResponse.BodyHandlers.ofString());
            assertEquals(200, registered.statusCode(), registered.body());
            assertEquals(json.readTree(registered.body()), ok(call(http, "/internal/consignments/get", "delivery", "trade", bytes(new RmbConsignmentApi.Get("901")))));
            assertEquals(404, call(http, "/internal/consignments/get", "delivery", "trade", bytes(new RmbConsignmentApi.Get("999999"))).statusCode());
            assertEquals(401, call(http, "/internal/consignments/get", "gateway", "201", bytes(new RmbConsignmentApi.Get("901"))).statusCode());
            assertEquals(409, http.send(signed, HttpResponse.BodyHandlers.ofString()).statusCode());
            assertEquals(json.readTree(registered.body()), json.readTree(call(http, "/internal/consignments/register", "delivery", "trade", input).body()));
            assertEquals(1, count("commerce_consignment"));

            var buying = new RmbConsignmentApi.Buy("901", "201", "原买家", "http-consignment-buy", "1");
            JsonNode order = ok(call(http, "/internal/consignments/buy", "delivery", "trade", bytes(buying)));
            String orderId = order.path("orderId").asText();
            assertEquals("AWAITING_PAYMENT", order.path("state").asText());
            assertEquals(order, ok(call(http, "/internal/consignments/buy", "delivery", "trade", bytes(buying))));
            assertEquals(409, call(http, "/internal/consignments/buy", "delivery", "trade",
                bytes(new RmbConsignmentApi.Buy("901", "201", "原买家", buying.requestId(), "3"))).statusCode(), "同账号ABA不能换原修订");
            assertEquals(400, call(http, "/internal/consignments/buy", "delivery", "trade",
                utf8(new String(bytes(buying), StandardCharsets.UTF_8).replace("\"buyerOwnerRevision\":\"1\"", "\"buyerOwnerRevision\":1"))).statusCode());
            assertEquals(404, call(http, "/internal/consignments/orders/get", "delivery", "trade", bytes(new RmbConsignmentApi.OrderQuery("过户后新账号", orderId))).statusCode());
            assertEquals(404, call(http, "/internal/consignments/orders/cancel", "delivery", "trade", bytes(new RmbConsignmentApi.OrderQuery("过户后新账号", orderId))).statusCode());
            assertEquals(order, ok(call(http, "/internal/consignments/orders/get", "delivery", "trade", bytes(new RmbConsignmentApi.OrderQuery("原买家", orderId)))));
            assertEquals(400, call(http, "/internal/consignments/buy", "delivery", "trade", utf8(new String(bytes(buying), StandardCharsets.UTF_8).replace("}", ",\"priceFen\":1}"))).statusCode());
            assertEquals(200, call(http, "/internal/mock/pay", "payment", "payment-adapter", bytes(new MockPaymentApi.Pay(orderId, "http-rmb-payment"))).statusCode());
            assertEquals(1, count("commerce_mock_transaction")); assertEquals(1, count("commerce_payment_receipt"));
            assertEquals(0, count("commerce_seller_income"));
            assertTrue(ok(call(http, "/internal/deliveries/poll", "delivery", "trade", bytes(new DeliveryApi.Poll(20)))).path("entries").isEmpty(),
                "人民币托管单不能混入官方SKU发物意图");
            var poll = ok(call(http, "/internal/consignments/deliveries/poll", "delivery", "trade", bytes(new DeliveryApi.Poll(20))));
            assertEquals(orderId, poll.path("entries").get(0).path("intent").path("orderId").asText());
            assertEquals("原买家", poll.path("entries").get(0).path("intent").path("buyerAccount").asText());
            assertEquals("1", poll.path("entries").get(0).path("intent").path("buyerOwnerRevision").asText());
            var receipt = new DeliveryApi.Receipt(orderId, "9901", "DELIVERED", "b".repeat(64));
            assertEquals(200, call(http, "/internal/deliveries/receipt", "delivery", "trade", bytes(receipt)).statusCode());
            assertEquals(200, call(http, "/internal/deliveries/receipt", "delivery", "trade", bytes(receipt)).statusCode());
            var income = ok(call(http, "/internal/consignments/incomes", "delivery", "trade", bytes(new RmbConsignmentApi.IncomePage("原卖家", "", 100)))).path("incomes").get(0);
            assertEquals(101, income.path("grossFen").asLong()); assertEquals(6, income.path("feeFen").asLong()); assertEquals(95, income.path("netFen").asLong());
            JsonNode withdrawal = ok(call(http, "/internal/consignments/withdrawals/create", "delivery", "trade", bytes(new RmbConsignmentApi.Withdraw("原卖家", "http-withdraw-request", 95))));
            String withdrawalId = withdrawal.path("withdrawalId").asText();
            byte[] payout = bytes(new MockPayoutStore.Pay(withdrawalId, "http-payout-request"));
            assertEquals(401, call(http, "/internal/mock/withdraw", "delivery", "trade", payout).statusCode());
            assertEquals(401, call(http, "/api/forward-payout", "gateway", "201", payout).statusCode());
            for (String bad : List.of("null", "[]", "{}", new String(payout, StandardCharsets.UTF_8) + " {}",
                "{\"withdrawalId\":\"" + withdrawalId + "\",\"requestId\":12345678}",
                "{\"withdrawalId\":\"" + withdrawalId + "\",\"requestId\":\"http-payout-request\",\"successful\":true}",
                "{\"withdrawalId\":\"" + withdrawalId + "\",\"requestId\":\"http-payout-request\",\"requestId\":\"replacement-request\"}")) {
                var rejected = call(http, "/internal/mock/withdraw", "payment", "payment-adapter", utf8(bad));
                assertEquals(400, rejected.statusCode(), bad + " => " + rejected.body());
            }
            assertEquals(0, count("commerce_mock_payout"));
            var paid = ok(call(http, "/internal/mock/withdraw", "payment", "payment-adapter", payout));
            assertEquals("PAID", paid.path("state").asText()); assertEquals(95, paid.path("amountFen").asLong());
            assertEquals(paid, ok(call(http, "/internal/mock/withdraw", "payment", "payment-adapter", payout)));
            assertEquals(paid, ok(call(http, "/internal/consignments/withdrawals/get", "delivery", "trade", bytes(new RmbConsignmentApi.WithdrawalQuery("原卖家", withdrawalId)))));
            assertEquals(404, call(http, "/internal/consignments/withdrawals/get", "delivery", "trade", bytes(new RmbConsignmentApi.WithdrawalQuery("原卖家 ", withdrawalId))).statusCode());
            assertEquals(1, count("commerce_mock_payout")); assertEquals(1, count("commerce_withdrawal_receipt"));
            assertEquals(95, jdbc.queryForObject("SELECT paid_fen FROM commerce_seller_income WHERE order_sn=?", Long.class, orderId));
            assertEquals(0, ok(call(http, "/internal/mock/withdrawals/reconcile", "payment", "payment-adapter", utf8("{}"))).path("attempted").asInt());
            assertEquals(400, call(http, "/internal/mock/withdrawals/reconcile", "payment", "payment-adapter", utf8("{\"limit\":1}")).statusCode());
            assertEquals(404, call(http, "/internal/consignments/unknown", "delivery", "trade", utf8("{}")).statusCode());
            assertEquals(404, call(http, "/internal/mock/withdrawals/unknown", "payment", "payment-adapter", utf8("{}")).statusCode());
        } finally {
            http.shutdownNow(); assertTrue(http.awaitTermination(Duration.ofSeconds(5)), "本例HTTP客户端未停止");
        }
    }

    private JsonNode ok(HttpResponse<String> response) throws Exception { assertEquals(200, response.statusCode(), response.body()); return json.readTree(response.body()); }
    private byte[] bytes(Object input) throws Exception { return json.writeValueAsBytes(input); }
    private static byte[] utf8(String input) { return input.getBytes(StandardCharsets.UTF_8); }
    private int count(String table) { return jdbc.queryForObject("SELECT COUNT(*) FROM " + table, Integer.class); }
    private HttpResponse<String> call(HttpClient http, String route, String role, String subject, byte[] body) throws Exception {
        return http.send(request(route, role, subject, body, PREFIX + route), HttpResponse.BodyHandlers.ofString());
    }
    private HttpRequest request(String route, String role, String subject, byte[] body, String signedPath) {
        String secret = switch (role) {
            case "payment" -> "rmb-http-payment-test-only-key-000002";
            case "delivery" -> "rmb-http-delivery-test-only-key-00003";
            case "admin" -> "rmb-http-admin-test-only-key-0000004";
            default -> "rmb-http-gateway-test-only-key-000001";
        };
        String timestamp = Long.toString(System.currentTimeMillis()), nonce = UUID.randomUUID().toString();
        return HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + PREFIX + route)).timeout(Duration.ofSeconds(20))
            .header("Content-Type", "application/json").header("X-Commerce-Subject", subject)
            .header("X-Commerce-Timestamp", timestamp).header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature", HmacAuthentication.sign(secret, role, subject, timestamp, nonce, "POST", signedPath, body))
            .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
    }
    private static String subject(String role) { return switch (role) { case "payment" -> "payment-adapter"; case "admin" -> "rmb-admin"; default -> "201"; }; }
    @TestConfiguration(proxyBeanMethods = false)
    static class TestEndpoints {
        @Bean @Primary Clock rmbTestClock() { return Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC); }
        @Bean ForwardController rmbForwardController() { return new ForwardController(); }
    }
    @RestController
    @org.springframework.boot.test.context.TestComponent
    static class ForwardController {
        @PostMapping("/api/forward-consignment")
        void consignment(HttpServletRequest request, HttpServletResponse response) throws ServletException, IOException {
            request.getRequestDispatcher("/rpc/internal/consignments/register").forward(request, response);
        }
        @PostMapping("/api/forward-payout")
        void payout(HttpServletRequest request, HttpServletResponse response) throws ServletException, IOException {
            request.getRequestDispatcher("/rpc/internal/mock/withdraw").forward(request, response);
        }
    }
}
