package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.boot.autoconfigure.jdbc.DataSourceProperties;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RestController;

/** 真HTTP覆盖角色隔离、双部署前缀和forward；实际TiDB另由随机库Store契约覆盖。 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1", "server.servlet.context-path=/mall", "spring.mvc.servlet.path=/rpc",
    "spring.datasource.url=jdbc:h2:mem:catalog_admin_http;MODE=MySQL;DB_CLOSE_DELAY=-1;LOCK_TIMEOUT=5000",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.allowed-payment-channels=test",
    "commerce.gateway-secret=catalog-gateway-test-only-key-000001",
    "commerce.payment-secret=catalog-payment-test-only-key-000002",
    "commerce.delivery-secret=catalog-delivery-test-only-key-00003",
    "commerce.admin-secret=catalog-admin-test-only-key-0000004", "commerce.admin-subject=catalog-admin"
})
@Import({CatalogAdminHttpTest.ForwardConfiguration.class, H2CommerceTestConfiguration.class})
class CatalogAdminHttpTest {
    private static final String PREFIX = "/mall/rpc";
    @LocalServerPort int port;
    @Autowired JdbcTemplate jdbc;
    @Autowired ObjectMapper json;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    @Test void H2适配不能接管真实SQL或生产强会话模式() {
        var factory = new H2CommerceTestConfiguration().h2CommerceDataSourceFactory();
        var source = new DataSourceProperties();
        var properties = new CommerceProperties();
        properties.setRunMode("test"); properties.setRequireTidb(false);
        source.setUrl("jdbc:mysql://127.0.0.1:24000/mall_it_test_only");
        assertThrows(IllegalStateException.class, () -> factory.create(source, properties));
        source.setUrl("jdbc:h2:mem:factory_guard");
        properties.setRunMode("prod");
        assertThrows(IllegalStateException.class, () -> factory.create(source, properties));
        properties.setRunMode("test"); properties.setRequireTidb(true);
        assertThrows(IllegalStateException.class, () -> factory.create(source, properties));
    }

    @Test void 管理命令签名与重放_保留预占和订单快照_不能直接改支付状态() throws Exception {
        var terms = new CatalogAdminApi.Terms(81, "admin-sku-811", 1250, "CNY", true, 10, 1, "玄铁");
        var create = new CatalogAdminApi.Create("admin-create-http", 811, terms, 5);
        for (String role : new String[] {"gateway", "payment", "delivery"}) {
            assertEquals(401, call("/internal/admin/skus/create", role, subject(role), create).statusCode());
        }
        assertEquals(401, call("/internal/admin/skus/create", "admin", "another-admin", create).statusCode());
        assertEquals(401, call("/api/forward-admin", "gateway", "33", create).statusCode());
        assertEquals(0, count("pms_sku_stock"));
        assertEquals(0, count("commerce_admin_command"));

        HttpRequest signed = request("/internal/admin/skus/create", "admin", "catalog-admin", create);
        var created = send(signed);
        assertEquals(200, created.statusCode(), created.body());
        var original = json.readTree(created.body());
        assertEquals(1, original.path("revision").asLong());
        assertEquals(409, send(signed).statusCode(), "认证nonce不能重放");
        assertEquals(original, json.readTree(call("/internal/admin/skus/create", "admin", "catalog-admin", create).body()));
        assertEquals(409, call("/internal/admin/skus/create", "admin", "catalog-admin",
            new CatalogAdminApi.Create(create.requestId(), 811, terms, 6)).statusCode());
        assertEquals(1, count("commerce_admin_command"));

        var bought = call("/api/orders", "gateway", "33", new CommerceApi.CreateOrder("admin-buyer-http", 811, 2, "测试账号", "1"));
        assertEquals(200, bought.statusCode(), bought.body());
        var order = json.readTree(bought.body());
        assertEquals(2500, order.path("amountMinor").asLong());
        var changedTerms = new CatalogAdminApi.Terms(81, "admin-sku-811", 2800, "CNY", true, 20, 2, "新玄铁");
        var changed = call("/internal/admin/skus/update", "admin", "catalog-admin",
            new CatalogAdminApi.Update("admin-update-http", 811, 1, changedTerms));
        assertEquals(200, changed.statusCode(), changed.body());
        assertEquals(2, json.readTree(changed.body()).path("revision").asLong());
        assertEquals(5, value("stock"));
        assertEquals(2, value("lock_stock"));
        String orderId = order.path("orderId").asText();
        var adminView = call("/internal/admin/order-view", "admin", "catalog-admin", new CommerceApi.OrderQuery(orderId));
        assertEquals(200, adminView.statusCode(), adminView.body());
        assertEquals(order, json.readTree(adminView.body()));
        assertEquals(401, call("/internal/admin/order-view", "payment", "payment-adapter", new CommerceApi.OrderQuery(orderId)).statusCode());
        var viewed = call("/internal/payments/order-view", "payment", "payment-adapter", new CommerceApi.OrderQuery(orderId));
        assertEquals(200, viewed.statusCode(), viewed.body());
        assertEquals(order, json.readTree(viewed.body()));
        assertEquals(10, jdbc.queryForObject("SELECT item_config_id FROM commerce_order_grant WHERE order_sn=?", Integer.class, orderId));
        assertEquals(1, jdbc.queryForObject("SELECT count_per_unit FROM commerce_order_grant WHERE order_sn=?", Integer.class, orderId));

        assertEquals(409, call("/internal/admin/skus/stock", "admin", "catalog-admin",
            new CatalogAdminApi.AdjustStock("admin-invalid-stock", 811, 2, -4)).statusCode());
        assertEquals(5, value("stock"));
        var stockInput = new CatalogAdminApi.AdjustStock("admin-stock-http", 811, 2, -3);
        var adjusted = call("/internal/admin/skus/stock", "admin", "catalog-admin", stockInput);
        assertEquals(200, adjusted.statusCode(), adjusted.body());
        assertEquals(2, value("stock"));
        var payment = new CommerceApi.Payment(orderId, "test", "catalog-http-paid", 2500, "CNY");
        assertEquals(401, call("/internal/payments", "admin", "catalog-admin", payment).statusCode());
        assertEquals(200, call("/internal/payments", "payment", "payment-adapter", payment).statusCode());
        assertEquals(0, value("stock"));
        assertEquals(0, value("lock_stock"));
        assertEquals(json.readTree(adjusted.body()),
            json.readTree(call("/internal/admin/skus/stock", "admin", "catalog-admin", stockInput).body()));
        assertEquals(0, value("stock"), "重放返回原响应，不能再次减少已支付库存");
        var page = call("/internal/admin/skus/list", "admin", "catalog-admin", new DeliveryApi.SkuPage(0, 10));
        assertEquals(200, page.statusCode(), page.body());
        assertEquals(1, json.readTree(page.body()).path("skus").size());
        assertEquals(0, json.readTree(page.body()).at("/skus/0/stock").asInt());
        assertEquals(3, count("commerce_admin_command"));
        assertEquals(1, count("commerce_payment_receipt"));
        assertEquals(1, count("commerce_delivery_intent"));
        assertEquals(0, count("commerce_delivery_receipt"));
        assertEquals(0, count("commerce_refund_receipt"));
        assertEquals(404, call("/internal/admin/skus/unknown", "admin", "catalog-admin", Map.of()).statusCode());
    }

    private HttpResponse<String> call(String route, String role, String subject, Object body) throws Exception {
        return send(request(route, role, subject, body));
    }
    private HttpRequest request(String route, String role, String subject, Object input) throws Exception {
        String path = PREFIX + route;
        byte[] body = json.writeValueAsBytes(input);
        String timestamp = Long.toString(System.currentTimeMillis());
        String nonce = UUID.randomUUID().toString();
        String secret = switch (role) {
            case "payment" -> "catalog-payment-test-only-key-000002";
            case "delivery" -> "catalog-delivery-test-only-key-00003";
            case "admin" -> "catalog-admin-test-only-key-0000004";
            default -> "catalog-gateway-test-only-key-000001";
        };
        return HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + path)).timeout(Duration.ofSeconds(8))
            .header("Content-Type", "application/json").header("X-Commerce-Subject", subject)
            .header("X-Commerce-Timestamp", timestamp).header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature", HmacAuthentication.sign(secret, role, subject, timestamp, nonce, "POST", path, body))
            .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
    }
    private HttpResponse<String> send(HttpRequest request) throws Exception {
        return http.send(request, HttpResponse.BodyHandlers.ofString());
    }
    private static String subject(String role) {
        return switch (role) { case "payment" -> "payment-adapter"; case "delivery" -> "trade"; default -> "33"; };
    }
    private int count(String table) { return jdbc.queryForObject("SELECT COUNT(*) FROM " + table, Integer.class); }
    private int value(String column) { return jdbc.queryForObject("SELECT " + column + " FROM pms_sku_stock WHERE id=811", Integer.class); }

    @TestConfiguration(proxyBeanMethods = false)
    static class ForwardConfiguration {
        @Bean ForwardController catalogForwardController() { return new ForwardController(); }
    }
    @RestController
    static class ForwardController {
        @PostMapping("/api/forward-admin")
        void forward(HttpServletRequest request, HttpServletResponse response) throws ServletException, IOException {
            request.getRequestDispatcher("/rpc/internal/admin/skus/create").forward(request, response);
        }
    }
}
