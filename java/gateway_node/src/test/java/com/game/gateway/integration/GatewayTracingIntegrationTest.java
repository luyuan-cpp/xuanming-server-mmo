package com.game.gateway.integration;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.dto.RefreshTokenRequest;
import com.game.gateway.dto.RefreshTokenResponse;
import com.game.gateway.grpc.LoginRpcClient;
import io.grpc.Metadata;
import io.grpc.MethodDescriptor;
import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ServerInterceptors;
import io.grpc.ServerServiceDefinition;
import io.grpc.stub.ServerCalls;
import io.micrometer.tracing.Span;
import io.micrometer.tracing.Tracer;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.actuate.observability.AutoConfigureObservability;
import org.springframework.boot.test.autoconfigure.web.servlet.AutoConfigureMockMvc;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.system.CapturedOutput;
import org.springframework.boot.test.system.OutputCaptureExtension;
import org.springframework.http.MediaType;
import org.springframework.test.context.ActiveProfiles;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.request.MockHttpServletRequestBuilder;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.UncheckedIOException;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/**
 * 网关的链路追踪,端到端核对线上真的写了什么(docs/design/distributed-tracing.md §3.3、§7)。
 *
 * <p>复用 {@link LoginEndpointIntegrationTest} 的 H2 上下文装配,只多打开追踪:Spring Boot 测试默认关追踪,
 * 由 {@code @AutoConfigureObservability} 打开。login.rpc 换成进程内的假服务,服务端拦截器记下每次调用收到的
 * {@code traceparent}。经过的是生产的 {@link LoginRpcClient} Bean、它的通道,以及 Spring Boot 的真实追踪装配
 * (HTTP 观测过滤器、OpenTelemetry 桥、MDC)。
 */
@SpringBootTest(classes = LoginEndpointIntegrationTest.H2HttpApplication.class)
@AutoConfigureMockMvc
@AutoConfigureObservability(metrics = false)
@ActiveProfiles("test")
@ExtendWith(OutputCaptureExtension.class)
class GatewayTracingIntegrationTest {

    private static final String LOGIN_SERVICE = "loginpb.ClientPlayerLogin";
    private static final Metadata.Key<String> TRACEPARENT =
            Metadata.Key.of("traceparent", Metadata.ASCII_STRING_MARSHALLER);
    /** W3C traceparent:第 1 组是 trace-id,第 2 组是 parent-id。 */
    private static final Pattern TRACEPARENT_FORMAT =
            Pattern.compile("00-([0-9a-f]{32})-([0-9a-f]{16})-0[01]");
    /** 客户端伪造的 trace-id,网关不能接。 */
    private static final String CLIENT_TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736";

    /** 「假服务名 方法全名」→ 最近一次收到的 traceparent;没带头记空串。 */
    private static final Map<String, String> received = new ConcurrentHashMap<>();
    private static Server staticLogin;

    @Autowired private MockMvc mockMvc;
    @Autowired private ObjectMapper objectMapper;
    @Autowired private LoginRpcClient loginRpcClient;
    @Autowired private Tracer tracer;

    /**
     * 上下文建起来之前先起假 login,静态通道(LoginRpcClient.start() 按 login.grpc.endpoints 建)指向它。
     * H2 换一个独立的库名,不和别的上下文(同一 JVM 里缓存着)共用 testdb,两边的 create-drop 互不干扰。
     */
    @DynamicPropertySource
    static void pointLoginAtFakeServer(DynamicPropertyRegistry registry) {
        staticLogin = startFakeLogin("static");
        registry.add("login.grpc.endpoints", () -> "1=127.0.0.1:" + staticLogin.getPort());
        registry.add("spring.datasource.url", () -> "jdbc:h2:mem:gateway_tracing;DB_CLOSE_DELAY=-1");
    }

    @AfterAll
    static void stopFakeLogin() {
        if (staticLogin != null) {
            staticLogin.shutdownNow();
        }
    }

    @Test
    void publicHttpEntryIgnoresClientTraceHeaders() throws Exception {
        received.clear();
        refreshViaHttp(post("/api/refresh-token")
                .header("traceparent", "00-" + CLIENT_TRACE_ID + "-00f067aa0ba902b7-01")
                .header("b3", CLIENT_TRACE_ID + "-00f067aa0ba902b7-1")
                .header("X-B3-TraceId", CLIENT_TRACE_ID)
                .header("X-B3-SpanId", "00f067aa0ba902b7")
                .header("X-B3-Sampled", "1"));

        // 出站 gRPC 带的是网关自己开的根,不是客户端给的 trace。
        Matcher onWire = traceparentOf("static " + LOGIN_SERVICE + "/RefreshToken");
        assertNotEquals(CLIENT_TRACE_ID, onWire.group(1));
    }

    @Test
    void everyPublicRequestStartsItsOwnRoot() throws Exception {
        received.clear();
        refreshViaHttp(post("/api/refresh-token"));
        String first = traceparentOf("static " + LOGIN_SERVICE + "/RefreshToken").group(1);
        refreshViaHttp(post("/api/refresh-token"));
        String second = traceparentOf("static " + LOGIN_SERVICE + "/RefreshToken").group(1);
        assertNotEquals(first, second);
    }

    @Test
    void outboundLoginRpcContinuesTheCallerTraceOnStaticAndDynamicChannels() {
        received.clear();
        Server dynamicLogin = startFakeLogin("dynamic");
        Span caller = tracer.nextSpan().name("caller").start();
        try (Tracer.SpanInScope ignored = tracer.withSpan(caller)) {
            // 先走静态通道:动态池为空时,RefreshToken 只在静态通道里轮询。
            loginRpcClient.refreshToken(new LoginRpcClient.RefreshTokenRequestProto("refresh"));
            // 再挂上 zone 1 的动态通道,带 zone 的 Login 优先走动态池。
            loginRpcClient.updateDynamicEndpoints(Map.of(1, List.of("127.0.0.1:" + dynamicLogin.getPort())));
            loginRpcClient.login(new LoginRpcClient.LoginRequestProto("account", "password", "password", ""), 1);
        } finally {
            caller.end();
            loginRpcClient.updateDynamicEndpoints(Map.of());
            dynamicLogin.shutdownNow();
        }

        for (String call : List.of("static " + LOGIN_SERVICE + "/RefreshToken", "dynamic " + LOGIN_SERVICE + "/Login")) {
            Matcher onWire = traceparentOf(call);
            assertEquals(caller.context().traceId(), onWire.group(1), call);
            // parent-id 是 gRPC client span,不是调用方自己的 span。
            assertNotEquals(caller.context().spanId(), onWire.group(2), call);
        }
    }

    @Test
    void jsonLogLinesCarryTraceIdAndSpanId(CapturedOutput output) {
        Span span = tracer.nextSpan().name("log-check").start();
        try (Tracer.SpanInScope ignored = tracer.withSpan(span)) {
            LoggerFactory.getLogger(GatewayTracingIntegrationTest.class).info("tracing log correlation check");
        } finally {
            span.end();
        }

        String line = output.getAll().lines()
                .filter(l -> l.contains("tracing log correlation check"))
                .findFirst()
                .orElseThrow(() -> new AssertionError("log line not captured"));
        assertTrue(line.contains("\"traceId\":\"" + span.context().traceId() + "\""), line);
        assertTrue(line.contains("\"spanId\":\"" + span.context().spanId() + "\""), line);
    }

    private void refreshViaHttp(MockHttpServletRequestBuilder request) throws Exception {
        RefreshTokenRequest body = new RefreshTokenRequest();
        body.setRefreshToken("old-refresh");
        mockMvc.perform(request
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(body)))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.code").value(RefreshTokenResponse.CODE_OK));
    }

    private static Matcher traceparentOf(String serverAndMethod) {
        String value = received.get(serverAndMethod);
        assertNotNull(value, "no call recorded for " + serverAndMethod + ", got " + received);
        Matcher matcher = TRACEPARENT_FORMAT.matcher(value);
        assertTrue(matcher.matches(), "not a W3C traceparent: '" + value + "'");
        return matcher;
    }

    private static Server startFakeLogin(String serverName) {
        try {
            return ServerBuilder.forPort(0)
                    .addService(ServerInterceptors.intercept(fakeLoginService(), recordTraceparent(serverName)))
                    .build()
                    .start();
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    /** Login 与 RefreshToken 都回空包:各字段取默认值,网关按成功处理。 */
    private static ServerServiceDefinition fakeLoginService() {
        ServerServiceDefinition.Builder service = ServerServiceDefinition.builder(LOGIN_SERVICE);
        for (String method : List.of("Login", "RefreshToken")) {
            MethodDescriptor<byte[], byte[]> descriptor = MethodDescriptor.<byte[], byte[]>newBuilder()
                    .setType(MethodDescriptor.MethodType.UNARY)
                    .setFullMethodName(LOGIN_SERVICE + "/" + method)
                    .setRequestMarshaller(ByteArrayMarshaller.INSTANCE)
                    .setResponseMarshaller(ByteArrayMarshaller.INSTANCE)
                    .build();
            service.addMethod(descriptor, ServerCalls.<byte[], byte[]>asyncUnaryCall((request, observer) -> {
                observer.onNext(new byte[0]);
                observer.onCompleted();
            }));
        }
        return service.build();
    }

    private static ServerInterceptor recordTraceparent(String serverName) {
        return new ServerInterceptor() {
            @Override
            public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(
                    ServerCall<ReqT, RespT> call, Metadata headers, ServerCallHandler<ReqT, RespT> next) {
                String traceparent = headers.get(TRACEPARENT);
                received.put(serverName + " " + call.getMethodDescriptor().getFullMethodName(),
                        traceparent == null ? "" : traceparent);
                return next.startCall(call, headers);
            }
        };
    }

    private enum ByteArrayMarshaller implements MethodDescriptor.Marshaller<byte[]> {
        INSTANCE;

        @Override
        public InputStream stream(byte[] value) {
            return new ByteArrayInputStream(value);
        }

        @Override
        public byte[] parse(InputStream stream) {
            try {
                return stream.readAllBytes();
            } catch (IOException e) {
                throw new UncheckedIOException(e);
            }
        }
    }
}
