package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;
import java.util.concurrent.atomic.AtomicBoolean;
import org.junit.jupiter.api.Test;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.web.context.WebServerInitializedEvent;

class InitializationTest {
    @Test void incompatibleSchemaPreventsHttpServerOpening() {
        try (var fixture = new CommerceFixture()) {
            fixture.jdbc.update("UPDATE commerce_schema_version SET storage_contract='untrusted-legacy-mall'");
            SpringApplication app = new SpringApplication(MallGameApplication.class, H2CommerceTestConfiguration.class);
            AtomicBoolean opened = new AtomicBoolean();
            app.addListeners(event -> { if (event instanceof WebServerInitializedEvent) { opened.set(true); } });
            Exception failure = assertThrows(Exception.class, () -> {
                try (var unexpected = app.run(
                        "--server.address=127.0.0.1", "--server.port=0", "--spring.main.banner-mode=off", "--logging.level.root=OFF",
                        "--spring.datasource.url=" + fixture.jdbcUrl, "--spring.datasource.username=", "--spring.datasource.password=",
                        "--spring.sql.init.mode=never", "--commerce.require-tidb=false", "--commerce.run-mode=test",
                        "--commerce.gateway-secret=startup-test-only-gateway-key-000000001", "--commerce.payment-secret=startup-test-only-payment-key-000000002")) {
                    fail("不兼容存储不应成功创建应用上下文");
                }
            });
            // Spring可包装装配异常；必须追到真实schema拒绝，不能让缺NORMAL等前置失败冒充本用例成功。
            Throwable cause = failure;
            while (cause.getCause() != null) { cause = cause.getCause(); }
            assertInstanceOf(IllegalStateException.class, cause);
            assertEquals("商城存储版本不兼容，必须使用独立迁移后的商城库", cause.getMessage());
            assertFalse(opened.get(), "不兼容存储必须在 WebServerInitializedEvent 之前拒绝");
            assertEquals(0, fixture.count("oms_order"));
        }
    }
}
