package com.game.gateway.service;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.ObjectReader;
import com.game.gateway.config.MallAdminProperties;
import io.micrometer.observation.Observation;
import io.micrometer.observation.ObservationRegistry;
import io.micrometer.observation.transport.RequestReplySenderContext;
import jakarta.annotation.PreDestroy;
import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.time.Clock;
import java.time.Duration;
import java.util.HexFormat;
import java.util.List;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Flow;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;
import org.springframework.web.server.ResponseStatusException;

/** 固定命令的HTTP适配器；不重试未知提交，调用方使用原requestId重放。 */
@Service
public final class MallAdminClient {
    private static final Set<String> COMMANDS = Set.of("skus/list", "skus/create", "skus/update", "skus/stock", "order-view");
    private static final int MAX_RESPONSE = 262_144;
    private final MallAdminProperties props;
    private final ObjectReader objects;
    private final HttpClient http;
    private final Clock clock;
    private final ObservationRegistry observations;
    @Autowired public MallAdminClient(MallAdminProperties props, ObjectMapper json, ObservationRegistry observations) {
        this(props, json, HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(3))
            .followRedirects(HttpClient.Redirect.NEVER).build(), Clock.systemUTC(), observations);
    }
    MallAdminClient(MallAdminProperties props, ObjectMapper json, HttpClient http, Clock clock, ObservationRegistry observations) {
        this.props = props; this.objects = json.readerFor(JsonNode.class).with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS);
        this.http = http; this.clock = clock; this.observations = observations;
    }
    @PreDestroy public void shutdown() { http.shutdownNow(); }
    public record Reply(int status, byte[] body) {}

    public Reply execute(String command, byte[] body) {
        if (!COMMANDS.contains(command)) { throw new ResponseStatusException(HttpStatus.NOT_FOUND, "管理命令不存在"); }
        if (!props.enabled()) { throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "商城管理代理未配置"); }
        if (body == null || body.length == 0 || body.length > 16_384) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "管理请求体为空或过大");
        }
        try {
            JsonNode input = objects.readValue(body);
            if (input == null || !input.isObject()) { throw new IOException("必须为JSON对象"); }
        } catch (IOException invalid) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "管理请求必须为JSON对象");
        }
        String base = props.getBaseUrl().replaceAll("/+$", "");
        URI uri = URI.create(base + "/internal/admin/" + command);
        String timestamp = Long.toString(clock.millis());
        String nonce = UUID.randomUUID().toString();
        HttpRequest.Builder request = HttpRequest.newBuilder(uri).timeout(Duration.ofSeconds(8))
            .header("Content-Type", "application/json").header("X-Commerce-Subject", props.getSubject())
            .header("X-Commerce-Timestamp", timestamp).header("X-Commerce-Nonce", nonce)
            .header("X-Commerce-Signature", signature(props.getSecret(), props.getSubject(), timestamp, nonce, uri.getRawPath(), body))
            .POST(HttpRequest.BodyPublishers.ofByteArray(body));
        // 出站调用开一个 client 观测(docs/design/distributed-tracing.md §7)：JDK HttpClient 不经 Spring 的观测，
        // 这里按 Micrometer 的发送端约定把请求构造器交给观测，Spring Boot 的追踪处理器在 start 时开 client span 并写入 traceparent。
        // 签名只覆盖上面几项，多一个传播头不影响验签。command 只有固定的几个值，可以做低基数标签。
        var context = new RequestReplySenderContext<HttpRequest.Builder, Integer>(HttpRequest.Builder::header);
        context.setCarrier(request);
        context.setRemoteServiceName("mall-admin");
        Observation observation = Observation.createNotStarted("mall.admin.client", () -> context, observations)
            .lowCardinalityKeyValue("command", command).start();
        try (Observation.Scope scope = observation.openScope()) {
            var pending = http.sendAsync(request.build(), ignored -> new LimitedBody());
            try {
                // future仅在受限BodySubscriber完成后才完成，8秒同时限制建连、头和整个正文。
                var response = pending.get(8, TimeUnit.SECONDS);
                int status = response.statusCode();
                context.setResponse(status);
                if (!Set.of(200, 400, 401, 404, 409, 413, 503).contains(status)) {
                    throw new IOException("上游返回非契约状态");
                }
                JsonNode parsed = objects.readValue(response.body());
                if (parsed == null || !parsed.isObject()) { throw new IOException("上游返回非JSON对象"); }
                return new Reply(status, response.body());
            } catch (InterruptedException cancelled) {
                pending.cancel(true);
                Thread.currentThread().interrupt();
                observation.error(cancelled);
                throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "商城管理请求已取消");
            } catch (TimeoutException | ExecutionException | IOException unavailable) {
                pending.cancel(true);
                observation.error(unavailable);
                throw new ResponseStatusException(HttpStatus.BAD_GATEWAY, "商城管理响应不可用，请使用原请求号核对结果");
            }
        } finally {
            observation.stop();
        }
    }

    static String signature(String secret, String subject, String timestamp, String nonce, String path, byte[] body) {
        try {
            String hash = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(body));
            String canonical = String.join("\n", "v1", "admin", subject, timestamp, nonce, "POST", path, hash);
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            return HexFormat.of().formatHex(mac.doFinal(canonical.getBytes(StandardCharsets.UTF_8)));
        } catch (GeneralSecurityException unavailable) { throw new IllegalStateException("JDK签名能力不可用", unavailable); }
    }

    /** 响应体也受总请求截止时间约束；超大小立即取消订阅，不接收无限错误页。 */
    private static final class LimitedBody implements HttpResponse.BodySubscriber<byte[]> {
        private final HttpResponse.BodySubscriber<byte[]> delegate = HttpResponse.BodySubscribers.ofByteArray();
        private Flow.Subscription subscription;
        private int received;
        private boolean failed;
        @Override public CompletionStage<byte[]> getBody() { return delegate.getBody(); }
        @Override public void onSubscribe(Flow.Subscription value) { subscription = value; delegate.onSubscribe(value); }
        @Override public void onNext(List<ByteBuffer> chunks) {
            if (failed) { return; }
            for (var chunk : chunks) {
                if (chunk.remaining() > MAX_RESPONSE - received) {
                    failed = true; subscription.cancel(); delegate.onError(new IOException("商城管理响应过大")); return;
                }
                received += chunk.remaining();
            }
            delegate.onNext(chunks);
        }
        @Override public void onError(Throwable error) { if (!failed) { failed = true; delegate.onError(error); } }
        @Override public void onComplete() { if (!failed) { delegate.onComplete(); } }
    }
}
