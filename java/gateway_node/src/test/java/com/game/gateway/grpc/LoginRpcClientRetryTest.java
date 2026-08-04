package com.game.gateway.grpc;

import com.game.gateway.config.LoginGrpcProperties;
import io.grpc.MethodDescriptor;
import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.ServerServiceDefinition;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.ServerCalls;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class LoginRpcClientRetryTest {

    private static final String LOGIN_SERVICE = "loginpb.ClientPlayerLogin";
    private static final String PREGATE_SERVICE = "loginpb.LoginPreGate";

    private final Map<String, AtomicInteger> calls = new ConcurrentHashMap<>();
    private Server server;
    private LoginRpcClient client;

    @BeforeEach
    void setUp() throws IOException {
        server = ServerBuilder.forPort(0)
                .addService(failingService(LOGIN_SERVICE, "Login", "RefreshToken"))
                .addService(failingService(PREGATE_SERVICE, "AssignGate", "QueryQueueStatus"))
                .build()
                .start();

        LoginGrpcProperties props = new LoginGrpcProperties();
        props.setEndpoints("1=127.0.0.1:" + server.getPort());
        props.setTimeoutMs(1000);
        props.setRetry(3);
        client = new LoginRpcClient(props);
        client.start();
    }

    @AfterEach
    void tearDown() {
        if (client != null) {
            client.stop();
        }
        if (server != null) {
            server.shutdownNow();
        }
    }

    @Test
    void sideEffectingRpcsNeverRetryAmbiguousUnavailableResponse() {
        assertUnavailable(() -> client.login(
                new LoginRpcClient.LoginRequestProto("account", "password", "password", ""), 1));
        assertUnavailable(() -> client.refreshToken(
                new LoginRpcClient.RefreshTokenRequestProto("refresh-token")));
        assertUnavailable(() -> client.assignGate(
                new LoginRpcClient.AssignGateRequestProto(1, "", "account", "device"), 1));
        assertUnavailable(() -> client.queryQueueStatus(
                new LoginRpcClient.QueryQueueStatusRequestProto("queue-token"), 1));

        assertEquals(1, callCount(LOGIN_SERVICE + "/Login"));
        assertEquals(1, callCount(LOGIN_SERVICE + "/RefreshToken"));
        assertEquals(1, callCount(PREGATE_SERVICE + "/AssignGate"));
        assertEquals(1, callCount(PREGATE_SERVICE + "/QueryQueueStatus"));
    }

    @Test
    void zoneBoundRpcsFailBeforeNetworkWhenMappingIsMissing() {
        assertMissingZone(() -> client.login(
                new LoginRpcClient.LoginRequestProto("account", "password", "password", ""), 2), 2);
        assertMissingZone(() -> client.assignGate(
                new LoginRpcClient.AssignGateRequestProto(2, "", "account", "device"), 2), 2);
        assertMissingZone(() -> client.queryQueueStatus(
                new LoginRpcClient.QueryQueueStatusRequestProto("queue-token"), 2), 2);

        // 缺 mapping 必须在选 channel 阶段失败，不能悄悄打到 zone 1。
        assertEquals(0, callCount(LOGIN_SERVICE + "/Login"));
        assertEquals(0, callCount(PREGATE_SERVICE + "/AssignGate"));
        assertEquals(0, callCount(PREGATE_SERVICE + "/QueryQueueStatus"));
    }

    @Test
    void zoneBoundRpcRejectsUnknownZeroZone() {
        assertThrows(IllegalArgumentException.class, () -> client.login(
                new LoginRpcClient.LoginRequestProto("account", "password", "password", ""), 0));
        assertEquals(0, callCount(LOGIN_SERVICE + "/Login"));
    }

    private void assertUnavailable(Runnable call) {
        StatusRuntimeException error = assertThrows(StatusRuntimeException.class, call::run);
        assertEquals(Status.Code.UNAVAILABLE, error.getStatus().getCode());
    }

    private void assertMissingZone(Runnable call, int zoneId) {
        IllegalStateException error = assertThrows(IllegalStateException.class, call::run);
        assertTrue(error.getMessage().contains("zone=" + zoneId));
    }

    private int callCount(String method) {
        AtomicInteger count = calls.get(method);
        return count == null ? 0 : count.get();
    }

    private ServerServiceDefinition failingService(String serviceName, String... methods) {
        ServerServiceDefinition.Builder service = ServerServiceDefinition.builder(serviceName);
        for (String methodName : methods) {
            String fullMethodName = serviceName + "/" + methodName;
            MethodDescriptor<byte[], byte[]> descriptor = MethodDescriptor.<byte[], byte[]>newBuilder()
                    .setType(MethodDescriptor.MethodType.UNARY)
                    .setFullMethodName(fullMethodName)
                    .setRequestMarshaller(ByteArrayMarshaller.INSTANCE)
                    .setResponseMarshaller(ByteArrayMarshaller.INSTANCE)
                    .build();
            service.addMethod(descriptor, ServerCalls.asyncUnaryCall((request, observer) -> {
                calls.computeIfAbsent(fullMethodName, ignored -> new AtomicInteger()).incrementAndGet();
                observer.onError(Status.UNAVAILABLE.withDescription("ambiguous fake failure").asRuntimeException());
            }));
        }
        return service.build();
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
                throw new RuntimeException(e);
            }
        }
    }
}
