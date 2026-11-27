package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;

/** 后台到商城管理入口:签名与商城 HmacAuthentication 同一规范串;确定的拒绝之外都记成结果未知,用原申请重试。 */
class GameMallAdminTest {
    private static final String SECRET = "mall-admin-test-only-secret-0000000001";
    final ObjectMapper mapper = new ObjectMapper();
    final List<JsonNode> bodies = new ArrayList<>();
    final List<String> problems = new ArrayList<>();
    final AtomicInteger status = new AtomicInteger(200);
    HttpServer server;
    GameMallAdmin mall;

    @BeforeEach void start() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/mall/rpc/internal/admin/", exchange -> {
            byte[] body = exchange.getRequestBody().readAllBytes();
            var headers = exchange.getRequestHeaders();
            String expected = GameMallAdmin.signature(SECRET, "mall-admin", headers.getFirst("X-Commerce-Timestamp"),
                    headers.getFirst("X-Commerce-Nonce"), exchange.getRequestURI().getRawPath(), body);
            if (!"mall-admin".equals(headers.getFirst("X-Commerce-Subject")) || !expected.equals(headers.getFirst("X-Commerce-Signature"))
                    || !headers.getFirst("X-Commerce-Nonce").matches("[A-Za-z0-9_-]{16,128}")) {
                problems.add("bad signature on " + exchange.getRequestURI());
            }
            bodies.add(mapper.readTree(body));
            byte[] reply = (status.get() == 200 ? "{\"resultingState\":\"DELIVERED\"}" : "{\"error\":\"order is not in MANUAL state\"}")
                    .getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(status.get(), reply.length);
            exchange.getResponseBody().write(reply);
            exchange.close();
        });
        server.start();
        mall = new GameMallAdmin(mapper, "http://127.0.0.1:" + server.getAddress().getPort() + "/mall/rpc", SECRET, "mall-admin",
                HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build());
    }

    @AfterEach void stop() { server.stop(0); }

    ObjectNode frozen() {
        ObjectNode frozen = mapper.createObjectNode().put("target", "mall").put("command", "manual/orders/resolve").put("requestId", "910000001");
        frozen.putObject("body").put("orderId", "MO-1").put("action", "MARK_DELIVERED").put("reason", "工单 88");
        return frozen;
    }

    ObjectNode approval() {
        return mapper.createObjectNode().put("approvalId", "910000001").put("proposer", "alice").put("approver", "bob").put("approvedAtMs", "1800000000000");
    }

    @Test void executeSignsTheRequestAndCarriesTheRequestIdAndTheApprovalRecord() {
        assertTrue(mall.enabled());
        assertEquals("DELIVERED", mall.execute(frozen(), approval()).path("resultingState").asText());
        assertEquals(List.of(), problems);
        JsonNode sent = bodies.get(0);
        assertEquals("910000001", sent.path("requestId").asText());
        assertEquals("MO-1", sent.path("orderId").asText());
        assertEquals("工单 88", sent.path("reason").asText());
        assertEquals("910000001", sent.path("approval").path("approvalId").asText());
        assertEquals("bob", sent.path("approval").path("approver").asText());
        assertTrue(sent.path("approval").path("approvedAtMs").isIntegralNumber());
        assertEquals(1_800_000_000_000L, sent.path("approval").path("approvedAtMs").asLong());
    }

    @Test void aDefiniteRefusalIsNotRetryableButAnUnavailableMallIsAnUnknownOutcome() {
        status.set(409);
        GameServices.CallFailure refused = assertThrows(GameServices.CallFailure.class, () -> mall.execute(frozen(), approval()));
        assertFalse(refused.isOutcomeUnknown());
        assertTrue(refused.getMessage().contains("order is not in MANUAL state"));
        status.set(503);
        assertTrue(assertThrows(GameServices.CallFailure.class, () -> mall.execute(frozen(), approval())).isOutcomeUnknown());
        // 读不回结果的只读查询只是没读到,不算未知。
        assertFalse(assertThrows(GameServices.CallFailure.class,
                () -> mall.read("manual/list", mapper.createObjectNode().put("limit", 10))).isOutcomeUnknown());
        // 连不上(端口 1 没有监听)同样可能是发出之后才断的:记成未知。
        GameMallAdmin unreachable = new GameMallAdmin(mapper, "http://127.0.0.1:1", SECRET, "mall-admin",
                HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build());
        assertTrue(assertThrows(GameServices.CallFailure.class, () -> unreachable.execute(frozen(), approval())).isOutcomeUnknown());
    }

    @Test void onlyCatalogedCommandsAndSafeAddressesAreAccepted() {
        ObjectNode unknown = frozen().put("command", "manual/list");
        assertFalse(assertThrows(GameServices.CallFailure.class, () -> mall.execute(unknown, approval())).isOutcomeUnknown());
        assertThrows(GameServices.CallFailure.class, () -> mall.read("skus/create", mapper.createObjectNode()));
        assertEquals(0, bodies.size());
        assertThrows(IllegalArgumentException.class, () -> new GameMallAdmin(mapper, "http://mall.internal:8080", SECRET, "mall-admin", null));
        assertThrows(IllegalArgumentException.class, () -> new GameMallAdmin(mapper, "https://user@mall.internal", SECRET, "mall-admin", null));
        assertFalse(new GameMallAdmin(mapper, "", SECRET, "mall-admin", null).enabled());
        assertFalse(new GameMallAdmin(mapper, "https://mall.internal", "short", "mall-admin", null).enabled());
    }

    @Test void plainHttpIsAllowedOnlyForLoopbackOrClusterServiceNames() {
        // 集群内服务间本来就是明文(设计 §11.4):回环与 <service>.<namespace>.svc[.cluster.local] 上的 http 放行,别的照旧拒绝。
        for (String url : List.of("http://mall.mmorpg.svc:8080/mall/rpc", "http://mall.mmorpg.svc.cluster.local:8080",
                "http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080")) {
            assertTrue(new GameMallAdmin(mapper, url, SECRET, "mall-admin", null).enabled(), url);
        }
        for (String url : List.of("http://mall.mmorpg:8080", "http://a.b.c.svc:8080", "http://x.svc.example.com:8080",
                "http://Mall.mmorpg.svc:8080", "http://10.0.0.8:8080", "http://[fe80::1]:8080", "http://user@mall.mmorpg.svc:8080",
                "http://mall.mmorpg.svc:8080?x=1", "http://mall.mmorpg.svc:8080#f")) {
            assertThrows(IllegalArgumentException.class, () -> new GameMallAdmin(mapper, url, SECRET, "mall-admin", null), url);
        }
    }
}
