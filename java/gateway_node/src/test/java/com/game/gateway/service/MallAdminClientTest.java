package com.game.gateway.service;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.config.MallAdminProperties;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.micrometer.observation.Observation;
import io.micrometer.observation.ObservationHandler;
import io.micrometer.observation.ObservationRegistry;
import io.micrometer.observation.transport.SenderContext;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import org.junit.jupiter.api.Test;
import org.springframework.web.server.ResponseStatusException;

class MallAdminClientTest {
    private static final String SECRET = "test-only-mall-admin-signing-key-00000001";
    private static final String SUBJECT = "gateway-admin";
    private static final String PREFIX = "/mall-context/servlet";
    private static final Clock CLOCK = Clock.fixed(Instant.ofEpochMilli(1_760_000_000_123L), ZoneOffset.UTC);
    private static final byte[] BODY = bytes("{\n  \"requestId\":\"stock-original-001\", \"skuId\":11,"
        + " \"expectedRevision\":2, \"delta\":3, \"title\":\"玄铁\"\n}");

    @Test void 固定命令保留原始字节和驼峰字段_签名绑定完整上游前缀() throws Exception {
        try (var fixture = new Upstream()) {
            byte[] response = bytes("{ \"skuId\":11, \"lockedStock\":2, \"priceMinor\":125 }");
            fixture.reply.set(new Response(200, response, null, false));
            for (String command : List.of("skus/list", "skus/create", "skus/update", "skus/stock", "order-view")) {
                var reply = fixture.client.execute(command, BODY);
                assertEquals(200, reply.status());
                assertArrayEquals(response, reply.body());
                var request = fixture.take();
                assertEquals("POST", request.method());
                assertEquals(PREFIX + "/internal/admin/" + command, request.path());
                assertNull(request.query());
                assertEquals("application/json", request.contentType());
                assertArrayEquals(BODY, request.body());
                assertEquals(SUBJECT, request.subject());
                assertEquals("1760000000123", request.timestamp());
                assertEquals(UUID.fromString(request.nonce()).toString(), request.nonce());
                assertEquals(sign(request, request.path(), request.body()), request.signature());
                assertNotEquals(sign(request, "/internal/admin/" + command, request.body()), request.signature(),
                    "不能剥掉商城的context或servlet前缀后签名");
                byte[] reserialized = new ObjectMapper().writeValueAsBytes(new ObjectMapper().readTree(BODY));
                assertNotEquals(sign(request, request.path(), reserialized), request.signature(), "签名须覆盖原字节");
            }
            assertEquals(5, fixture.calls.get());
            fixture.assertHealthy();
        }
    }

    @Test void 重放使用新传输nonce但保留原业务请求号_每次只发一次() throws Exception {
        try (var fixture = new Upstream()) {
            fixture.reply.set(new Response(503, bytes("{\"error\":\"commit-unknown\"}"), null, false));
            var first = fixture.client.execute("skus/stock", BODY);
            var firstRequest = fixture.take();
            assertEquals(503, first.status());
            assertEquals(1, fixture.calls.get(), "未知提交不自动重试");
            fixture.reply.set(new Response(409, bytes("{\"error\":\"revision-conflict\"}"), null, false));
            var second = fixture.client.execute("skus/stock", BODY);
            var secondRequest = fixture.take();
            assertEquals(409, second.status());
            assertEquals(2, fixture.calls.get());
            assertNotEquals(firstRequest.nonce(), secondRequest.nonce());
            assertArrayEquals(BODY, firstRequest.body());
            assertArrayEquals(firstRequest.body(), secondRequest.body());
            assertEquals("stock-original-001", new ObjectMapper().readTree(secondRequest.body()).get("requestId").asText());
            fixture.assertHealthy();
        }
    }

    @Test void 正式构造器禁止跟随重定向_拒绝状态且不请求目标() throws Exception {
        try (var fixture = new Upstream()) {
            fixture.reply.set(new Response(302, bytes("{\"redirect\":true}"), "/redirect-target", false));
            var publicClient = fixture.defaultClient();
            assertEquals(502, assertThrows(ResponseStatusException.class,
                () -> publicClient.execute("skus/create", BODY)).getStatusCode().value());
            assertEquals(1, fixture.calls.get());
            assertEquals(PREFIX + "/internal/admin/skus/create", fixture.take().path());
            assertTrue(fixture.requests.isEmpty());
            fixture.assertHealthy();
        }
    }

    @Test void 非JSON对象_尾随垃圾或双根响应一律拒绝且不重试() throws Exception {
        try (var fixture = new Upstream()) {
            int count = 0;
            for (String raw : List.of("not-json", "<html>error</html>", "null", "[]", "\"text\"",
                    "{\"ok\":true} junk", "{\"ok\":true} {\"second\":true}")) {
                fixture.reply.set(new Response(200, bytes(raw), null, false));
                assertEquals(502, assertThrows(ResponseStatusException.class,
                    () -> fixture.client.execute("skus/stock", BODY)).getStatusCode().value(), raw);
                assertEquals(++count, fixture.calls.get());
                assertArrayEquals(BODY, fixture.take().body());
            }
            fixture.assertHealthy();
        }
    }

    @Test void 响应大小上界可接收_超一字节取消且不返回截断JSON() throws Exception {
        try (var fixture = new Upstream()) {
            byte[] maximum = bytes("{\"padding\":\"" + "x".repeat(262_144 - 14) + "\"}");
            assertEquals(262_144, maximum.length);
            fixture.reply.set(new Response(200, maximum, null, false));
            assertArrayEquals(maximum, fixture.client.execute("skus/list", bytes("{}")).body());
            fixture.take();
            byte[] excessive = bytes("{\"padding\":\"" + "x".repeat(262_145 - 14) + "\"}");
            fixture.reply.set(new Response(200, excessive, null, true));
            assertEquals(502, assertThrows(ResponseStatusException.class,
                () -> fixture.client.execute("skus/list", bytes("{}"))).getStatusCode().value());
            assertEquals(2, fixture.calls.get());
            fixture.take();
            fixture.assertHealthy();
        }
    }

    @Test void 非固定命令或非法请求体在网络前拒绝_禁用配置保持关闭() throws Exception {
        try (var fixture = new Upstream()) {
            for (String command : List.of("skus/delete", "../order-view", "skus/create?target=x", "")) {
                assertEquals(404, assertThrows(ResponseStatusException.class,
                    () -> fixture.client.execute(command, BODY)).getStatusCode().value());
            }
            for (byte[] body : new byte[][] {null, new byte[0], bytes("null"), bytes("[]"), bytes("{} junk"),
                    bytes("{} {}"), new byte[16_385]}) {
                assertEquals(400, assertThrows(ResponseStatusException.class,
                    () -> fixture.client.execute("skus/create", body)).getStatusCode().value());
            }
            var disabled = new MallAdminProperties();
            disabled.validate();
            var client = fixture.client(disabled);
            assertEquals(503, assertThrows(ResponseStatusException.class,
                () -> client.execute("skus/create", BODY)).getStatusCode().value());
            assertEquals(0, fixture.calls.get());
            fixture.assertHealthy();
        }
    }

    @Test void 启用配置必须有合法固定地址与独立签名材料() throws Exception {
        try (var fixture = new Upstream()) {
            for (String target : List.of("file:///tmp/mall", "http://user:pass@127.0.0.1:1", "http://127.0.0.1:1/base?x=1",
                    "http://127.0.0.1:1/base#fragment", "http://127.0.0.1:1/a%2fb", "http://127.0.0.1:1/a/../b")) {
                var props = fixture.properties();
                props.setBaseUrl(target);
                assertThrows(IllegalStateException.class, props::validate);
            }
            var shortKey = fixture.properties();
            shortKey.setSecret("short");
            assertThrows(IllegalStateException.class, shortKey::validate);
            var wrongSubject = fixture.properties();
            wrongSubject.setSubject("ADMIN");
            assertThrows(IllegalStateException.class, wrongSubject::validate);
            assertEquals(0, fixture.calls.get());
        }
    }

    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }

    private static String sign(Request request, String path, byte[] body) throws Exception {
        String digest = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(body));
        String canonical = "v1\nadmin\n" + SUBJECT + "\n" + request.timestamp() + "\n" + request.nonce()
            + "\nPOST\n" + path + "\n" + digest;
        Mac mac = Mac.getInstance("HmacSHA256");
        mac.init(new SecretKeySpec(bytes(SECRET), "HmacSHA256"));
        return HexFormat.of().formatHex(mac.doFinal(bytes(canonical)));
    }

    /** 出站观测的发送端约定(docs/design/distributed-tracing.md §7)：观测 start 时经载体写入的传播头会随请求上线，签名照常可验。 */
    @Test void 观测写入的traceparent随请求上线_签名不受影响_无观测时不带() throws Exception {
        var observations = ObservationRegistry.create();
        observations.observationConfig().observationHandler(new InjectingSenderHandler());
        try (var fixture = new Upstream(observations)) {
            fixture.reply.set(new Response(200, bytes("{}"), null, false));
            assertEquals(200, fixture.client.execute("skus/list", BODY).status());
            var request = fixture.take();
            assertEquals(TRACEPARENT, request.traceparent());
            assertEquals(sign(request, request.path(), request.body()), request.signature());
            fixture.assertHealthy();
        }
        try (var fixture = new Upstream()) {
            fixture.reply.set(new Response(200, bytes("{}"), null, false));
            assertEquals(200, fixture.client.execute("skus/list", BODY).status());
            assertNull(fixture.take().traceparent(), "NOOP 注册表不开观测，也不写传播头");
            fixture.assertHealthy();
        }
    }

    private static final String TRACEPARENT = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

    /** 只做 Spring Boot 的 PropagatingSenderTracingObservationHandler 注入那一步：start 时往载体写固定的 traceparent。 */
    private static final class InjectingSenderHandler implements ObservationHandler<SenderContext<?>> {
        @Override public void onStart(SenderContext<?> context) { inject(context); }
        private static <C> void inject(SenderContext<C> context) {
            context.getSetter().set(context.getCarrier(), "traceparent", TRACEPARENT);
        }
        @Override public boolean supportsContext(Observation.Context context) { return context instanceof SenderContext; }
    }

    private record Request(String method, String path, String query, byte[] body, String contentType,
        String subject, String timestamp, String nonce, String signature, String traceparent) {}
    private record Response(int status, byte[] body, String location, boolean clientMayCancel) {}

    private static final class Upstream implements AutoCloseable {
        final HttpServer server;
        final ExecutorService serverExecutor;
        final ExecutorService httpExecutor;
        final HttpClient http;
        final ObservationRegistry observations;
        final MallAdminClient client;
        final List<MallAdminClient> clients = new ArrayList<>();
        final AtomicReference<Response> reply = new AtomicReference<>(new Response(200, bytes("{}"), null, false));
        final AtomicInteger calls = new AtomicInteger();
        final LinkedBlockingQueue<Request> requests = new LinkedBlockingQueue<>();
        final ConcurrentLinkedQueue<Throwable> failures = new ConcurrentLinkedQueue<>();

        Upstream() throws IOException {
            this(ObservationRegistry.NOOP);
        }

        Upstream(ObservationRegistry observations) throws IOException {
            this.observations = observations;
            server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            serverExecutor = Executors.newFixedThreadPool(2);
            httpExecutor = Executors.newFixedThreadPool(2);
            try {
                server.setExecutor(serverExecutor);
                server.createContext("/", this::handle);
                server.start();
                http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3)).followRedirects(HttpClient.Redirect.NEVER)
                    .version(HttpClient.Version.HTTP_1_1).executor(httpExecutor).build();
                client = client(properties());
            } catch (RuntimeException | Error failure) {
                server.stop(0);
                serverExecutor.shutdownNow();
                httpExecutor.shutdownNow();
                throw failure;
            }
        }

        MallAdminProperties properties() {
            var props = new MallAdminProperties();
            props.setBaseUrl("http://127.0.0.1:" + server.getAddress().getPort() + PREFIX + "/");
            props.setSubject(SUBJECT);
            props.setSecret(SECRET);
            props.validate();
            return props;
        }

        MallAdminClient client(MallAdminProperties props) {
            var value = new MallAdminClient(props, new ObjectMapper(), http, CLOCK, observations);
            clients.add(value);
            return value;
        }

        MallAdminClient defaultClient() {
            var value = new MallAdminClient(properties(), new ObjectMapper(), observations);
            clients.add(value);
            return value;
        }

        Request take() throws InterruptedException {
            var request = requests.poll(2, TimeUnit.SECONDS);
            assertNotNull(request, "已返回的HTTP调用缺少服务端捕获原件");
            return request;
        }

        void assertHealthy() { assertTrue(failures.isEmpty(), () -> "自有HTTP夹具异常：" + failures); }

        private void handle(HttpExchange exchange) {
            var response = reply.get();
            try (exchange) {
                byte[] body = exchange.getRequestBody().readNBytes(16_385);
                var headers = exchange.getRequestHeaders();
                calls.incrementAndGet();
                requests.add(new Request(exchange.getRequestMethod(), exchange.getRequestURI().getRawPath(),
                    exchange.getRequestURI().getRawQuery(), body, headers.getFirst("Content-Type"),
                    headers.getFirst("X-Commerce-Subject"), headers.getFirst("X-Commerce-Timestamp"),
                    headers.getFirst("X-Commerce-Nonce"), headers.getFirst("X-Commerce-Signature"),
                    headers.getFirst("traceparent")));
                exchange.getResponseHeaders().set("Content-Type", "application/json");
                if (response.location() != null) { exchange.getResponseHeaders().set("Location", response.location()); }
                try {
                    exchange.sendResponseHeaders(response.status(), response.body().length);
                    exchange.getResponseBody().write(response.body());
                } catch (IOException cancelled) {
                    if (!response.clientMayCancel()) { failures.add(cancelled); }
                }
            } catch (Throwable failure) { failures.add(failure); }
        }

        @Override public void close() throws InterruptedException {
            server.stop(0);
            for (var value : clients) { value.shutdown(); }
            serverExecutor.shutdownNow();
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
            boolean clientStopped;
            // 先允许HttpClient排空自身取消回调，再停它的执行器，避免清理顺序制造假泄漏。
            try { clientStopped = http.awaitTermination(Duration.ofNanos(Math.max(1, deadline - System.nanoTime()))); }
            finally { httpExecutor.shutdownNow(); }
            boolean serverStopped = serverExecutor.awaitTermination(Math.max(1, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
            boolean executorStopped = httpExecutor.awaitTermination(Math.max(1, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
            assertAll(() -> assertTrue(clientStopped, "自有HTTP client未退出"),
                () -> assertTrue(serverStopped, "自有HTTP server executor未退出"),
                () -> assertTrue(executorStopped, "自有HTTP client executor未退出"), this::assertHealthy);
        }
    }
}
