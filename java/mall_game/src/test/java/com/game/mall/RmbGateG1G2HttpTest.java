package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.context.annotation.Primary;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/**
 * 真实 Loopback HTTP 验证真钱聚宝斋门禁 G-1（trade:&lt;uint64&gt; 跨端托管引用、分层公示与定向买家）
 * 与 G-2（逐单争议冻结/释放与提现同账号锁裁决）。
 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1", "server.servlet.context-path=/mall", "spring.mvc.servlet.path=/rpc",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.mock-payment-enabled=true",
    "commerce.consignment.notice-ms=172800000",
    "commerce.consignment.small-batch-max-price-fen=5000",
    "commerce.consignment.small-batch-notice-ms=0",
    "commerce.consignment.cooling-ms=0",
    "commerce.gateway-secret=rmb-g1g2-gateway-test-only-key-001",
    "commerce.payment-secret=rmb-g1g2-payment-test-only-key-002",
    "commerce.delivery-secret=rmb-g1g2-delivery-test-only-key-03",
    "commerce.admin-secret=rmb-g1g2-admin-test-only-key-00004", "commerce.admin-subject=g1g2-admin"
})
@Import({RmbGateG1G2HttpTest.TestClockConfig.class, H2CommerceTestConfiguration.class})
@DirtiesContext(classMode = DirtiesContext.ClassMode.AFTER_CLASS)
class RmbGateG1G2HttpTest {
    private static final String PREFIX = "/mall/rpc";
    private static final String DATABASE = "rmb_g1g2_http_" + UUID.randomUUID().toString().replace("-", "");
    private static final long FIXED_NOW_MS = 1_760_000_000_000L;

    @DynamicPropertySource
    static void database(DynamicPropertyRegistry registry) {
        registry.add("spring.datasource.url",
            () -> "jdbc:h2:mem:" + DATABASE + ";MODE=MySQL;DATABASE_TO_LOWER=TRUE;LOCK_TIMEOUT=5000");
    }

    @LocalServerPort int port;
    @Autowired ObjectMapper json;

    @Test
    void G1跨端trade引用与分层公示定向买家_G2争议冻结拦截提现并释放恢复() throws Exception {
        var http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3))
            .followRedirects(HttpClient.Redirect.NEVER).build();
        try {
            // 1) G-1：高价角色寄售使用 Go 侧 "trade:<uint64>" 托管引用，命中完整 48 小时公示期，并验证退回回执。
            var charEscrow = new RmbConsignmentApi.Escrow("8801", "1001", "角色卖家", 1, "CHARACTER", null,
                "trade:8801", "c".repeat(64), 3000, "满级道童", null);
            JsonNode charListed = ok(call(http, "/internal/consignments/register", "delivery", "trade", bytes(charEscrow)));
            assertEquals(FIXED_NOW_MS + 172_800_000L, charListed.path("noticeEndMs").asLong(), "角色寄售即使低于50元也必须走完整48h公示");
            assertEquals("PUBLIC_NOTICE", charListed.path("phase").asText());
            assertEquals(409, call(http, "/internal/consignments/buy", "delivery", "trade",
                bytes(new RmbConsignmentApi.Buy("8801", "2001", "路人买家", "buy-during-notice", "1"))).statusCode());
            JsonNode cancelled = ok(call(http, "/internal/consignments/cancel", "delivery", "trade",
                bytes(new RmbConsignmentApi.Cancel("8801", "角色卖家"))));
            assertEquals("RETURNING", cancelled.path("state").asText());
            var returnsPoll = ok(call(http, "/internal/consignments/returns/poll", "delivery", "trade", bytes(new DeliveryApi.Poll(20))));
            assertEquals("trade:8801", returnsPoll.path("entries").get(0).path("intent").path("escrowReference").asText());
            assertEquals(200, call(http, "/internal/consignments/returns/receipt", "delivery", "trade",
                bytes(new RmbConsignmentApi.ReturnReceipt("8801", "trade:8801", "c".repeat(64), "d".repeat(64)))).statusCode());

            // 2) G-1 + 分层公示 + 官方指定买家定向单：小额 ITEMS（200分 <= 5000分）免48h等待立即在售，仅允许指定买家下单。
            var smallDesignated = new RmbConsignmentApi.Escrow("8802", "1002", "材料卖家", 1, "ITEMS", null,
                "trade:8802", "e".repeat(64), 200, "玄铁材料包", "定向买家");
            JsonNode smallListed = ok(call(http, "/internal/consignments/register", "delivery", "trade", bytes(smallDesignated)));
            assertEquals(FIXED_NOW_MS, smallListed.path("noticeEndMs").asLong(), "小额标准材料包命中短公示(0ms)");
            assertEquals("ON_SALE", smallListed.path("phase").asText());
            assertEquals("定向买家", smallListed.path("designatedBuyerAccount").asText());

            assertEquals(409, call(http, "/internal/consignments/buy", "delivery", "trade",
                bytes(new RmbConsignmentApi.Buy("8802", "2001", "路人买家", "stranger-buy-8802", "1"))).statusCode(),
                "非指定买家不得抢拍定向托管单");
            JsonNode order = ok(call(http, "/internal/consignments/buy", "delivery", "trade",
                bytes(new RmbConsignmentApi.Buy("8802", "2002", "定向买家", "designated-buy-8802", "1"))));
            String orderId = order.path("orderId").asText();

            assertEquals(200, call(http, "/internal/mock/pay", "payment", "payment-adapter",
                bytes(new MockPaymentApi.Pay(orderId, "pay-8802"))).statusCode());
            var deliveryPoll = ok(call(http, "/internal/consignments/deliveries/poll", "delivery", "trade", bytes(new DeliveryApi.Poll(20))));
            assertEquals("trade:8802", deliveryPoll.path("entries").get(0).path("intent").path("escrowReference").asText());
            assertEquals(200, call(http, "/internal/deliveries/receipt", "delivery", "trade",
                bytes(new DeliveryApi.Receipt(orderId, "8802", "DELIVERED", "f".repeat(64)))).statusCode());

            // 3) G-2：逐单争议冻结与提现共享账号锁：gross=200, fee=10, net=190；冻结 150 后仅可提现 40，释放后可提现剩余 150。
            // 后台下发的人工处置带双人复核批准记录,请求号就是批准号。
            var freezeReq = new ManualAdminApi.ResolveIncomeDispute("880000001", orderId, "FREEZE_DISPUTE", 150, "CASE-HTTP-G2", "渠道拒付协查",
                new AdminApproval("880000001", "alice", "bob", 1_700_000_000_000L));
            assertEquals(401, call(http, "/internal/admin/manual/incomes/dispute", "delivery", "trade", bytes(freezeReq)).statusCode(),
                "非管理员角色禁止操作逐单争议冻结");
            JsonNode frozenRes = ok(call(http, "/internal/admin/manual/incomes/dispute", "admin", "g1g2-admin", bytes(freezeReq)));
            assertEquals("FREEZE_DISPUTE", frozenRes.path("action").asText());

            JsonNode incomeFrozen = ok(call(http, "/internal/consignments/incomes", "delivery", "trade",
                bytes(new RmbConsignmentApi.IncomePage("材料卖家", "", 20)))).path("incomes").get(0);
            assertEquals(190, incomeFrozen.path("netFen").asLong());
            assertEquals(150, incomeFrozen.path("disputeFrozenFen").asLong());
            assertEquals("CASE-HTTP-G2", incomeFrozen.path("disputeCaseId").asText());

            assertEquals(409, call(http, "/internal/consignments/withdrawals/create", "delivery", "trade",
                bytes(new RmbConsignmentApi.Withdraw("材料卖家", "withdraw-over-frozen", 41))).statusCode(),
                "超过未冻结净额(190-150=40)的提现必须被拒");
            JsonNode partialWithdraw = ok(call(http, "/internal/consignments/withdrawals/create", "delivery", "trade",
                bytes(new RmbConsignmentApi.Withdraw("材料卖家", "withdraw-unfrozen-40", 40))));
            assertEquals(40, partialWithdraw.path("amountFen").asLong());

            var releaseReq = new ManualAdminApi.ResolveIncomeDispute("880000002", orderId, "RELEASE_DISPUTE", 150, "CASE-HTTP-G2", "申诉核实无误释放",
                new AdminApproval("880000002", "carol", "bob", 1_700_000_000_000L));
            ok(call(http, "/internal/admin/manual/incomes/dispute", "admin", "g1g2-admin", bytes(releaseReq)));

            JsonNode incomeReleased = ok(call(http, "/internal/consignments/incomes", "delivery", "trade",
                bytes(new RmbConsignmentApi.IncomePage("材料卖家", "", 20)))).path("incomes").get(0);
            assertEquals(0, incomeReleased.path("disputeFrozenFen").asLong());

            JsonNode restWithdraw = ok(call(http, "/internal/consignments/withdrawals/create", "delivery", "trade",
                bytes(new RmbConsignmentApi.Withdraw("材料卖家", "withdraw-rest-150", 150))));
            assertEquals(150, restWithdraw.path("amountFen").asLong());
        } finally {
            http.shutdownNow();
            assertTrue(http.awaitTermination(Duration.ofSeconds(5)));
        }
    }

    private JsonNode ok(HttpResponse<String> response) throws Exception {
        assertEquals(200, response.statusCode(), response.body());
        return json.readTree(response.body());
    }

    private byte[] bytes(Object input) throws Exception {
        return json.writeValueAsBytes(input);
    }

    private HttpResponse<String> call(HttpClient http, String route, String role, String subject, byte[] body) throws Exception {
        String secret = switch (role) {
            case "payment" -> "rmb-g1g2-payment-test-only-key-002";
            case "delivery" -> "rmb-g1g2-delivery-test-only-key-03";
            case "admin" -> "rmb-g1g2-admin-test-only-key-00004";
            default -> "rmb-g1g2-gateway-test-only-key-001";
        };
        String timestamp = Long.toString(System.currentTimeMillis());
        String nonce = UUID.randomUUID().toString();
        String signedPath = PREFIX + route;
        HttpRequest req = HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + signedPath))
            .timeout(Duration.ofSeconds(20))
            .header("Content-Type", "application/json")
            .header("X-Commerce-Subject", subject)
            .header("X-Commerce-Timestamp", timestamp)
            .header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature", HmacAuthentication.sign(secret, role, subject, timestamp, nonce, "POST", signedPath, body))
            .POST(HttpRequest.BodyPublishers.ofByteArray(body))
            .build();
        return http.send(req, HttpResponse.BodyHandlers.ofString());
    }

    @TestConfiguration(proxyBeanMethods = false)
    static class TestClockConfig {
        @Bean
        @Primary
        Clock g1g2TestClock() {
            return Clock.fixed(Instant.ofEpochMilli(FIXED_NOW_MS), ZoneOffset.UTC);
        }
    }
}
