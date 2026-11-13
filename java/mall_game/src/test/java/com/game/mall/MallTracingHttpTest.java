package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import io.micrometer.tracing.Span;
import io.micrometer.tracing.Tracer;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.test.system.CapturedOutput;
import org.springframework.boot.test.system.OutputCaptureExtension;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RestController;

/**
 * 商城接住调用方的 trace(docs/design/distributed-tracing.md §7):trade、网关调商城时带 traceparent,
 * 商城的观测在它下面继续,日志带同一个 traceId。探针接口只在本用例里注册,照常经过 HMAC 认证。
 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "server.address=127.0.0.1",
    "spring.datasource.url=jdbc:h2:mem:mall_tracing_http;MODE=MySQL;DB_CLOSE_DELAY=-1;LOCK_TIMEOUT=5000",
    "spring.datasource.username=sa", "spring.datasource.password=", "spring.sql.init.mode=always",
    "spring.sql.init.schema-locations=classpath:db/schema.sql", "commerce.require-tidb=false", "commerce.run-mode=test",
    "commerce.allowed-payment-channels=test",
    "commerce.gateway-secret=" + MallTracingHttpTest.GATEWAY_SECRET,
    "commerce.payment-secret=tracing-payment-test-only-key-000002",
    "commerce.delivery-secret=tracing-delivery-test-only-key-00003",
    "commerce.admin-secret=tracing-admin-test-only-key-0000004"
})
@Import({MallTracingHttpTest.ProbeConfiguration.class, H2CommerceTestConfiguration.class})
@ExtendWith(OutputCaptureExtension.class)
class MallTracingHttpTest {
    static final String GATEWAY_SECRET = "tracing-gateway-test-only-key-000001";
    private static final String PROBE_PATH = "/api/trace-probe";
    private static final String TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736";
    private static final String PLAYER = "33";

    @LocalServerPort int port;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).build();

    @Test void 调用方的traceparent被接住_日志带同一个traceId(CapturedOutput output) throws Exception {
        byte[] body = "{}".getBytes(StandardCharsets.UTF_8);
        String timestamp = Long.toString(System.currentTimeMillis());
        String nonce = UUID.randomUUID().toString();
        HttpRequest request = HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + PROBE_PATH))
            .timeout(Duration.ofSeconds(5))
            .header("Content-Type", "application/json")
            .header("X-Commerce-Subject", PLAYER)
            .header("X-Commerce-Timestamp", timestamp)
            .header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature",
                HmacAuthentication.sign(GATEWAY_SECRET, "gateway", PLAYER, timestamp, nonce, "POST", PROBE_PATH, body))
            .header("traceparent", "00-" + TRACE_ID + "-00f067aa0ba902b7-01")
            .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();

        HttpResponse<String> response = http.send(request, HttpResponse.BodyHandlers.ofString());

        assertEquals(200, response.statusCode(), response.body());
        assertEquals(TRACE_ID, response.body(), "商城的当前 span 要接在调用方的 trace 下");
        assertTrue(output.getOut().contains("\"traceId\":\"" + TRACE_ID + "\""), "探针打的日志要带同一个 traceId");
    }

    @TestConfiguration(proxyBeanMethods = false)
    static class ProbeConfiguration {
        @Bean TraceProbeController traceProbeController(Tracer tracer) { return new TraceProbeController(tracer); }
    }

    /** 回当前 span 的 traceId,并打一行日志,供用例核对传播与 MDC。 */
    @RestController
    static class TraceProbeController {
        private static final Logger LOG = LoggerFactory.getLogger(TraceProbeController.class);
        private final Tracer tracer;

        TraceProbeController(Tracer tracer) { this.tracer = tracer; }

        @PostMapping(PROBE_PATH)
        String probe() {
            LOG.info("trace probe");
            Span span = tracer.currentSpan();
            return span == null ? "" : span.context().traceId();
        }
    }
}
