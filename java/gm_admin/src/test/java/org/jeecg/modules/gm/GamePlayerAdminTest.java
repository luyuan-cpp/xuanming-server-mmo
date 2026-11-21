package org.jeecg.modules.gm;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.util.JsonFormat;
import com.sun.net.httpserver.HttpServer;
import io.grpc.Server;
import io.grpc.ServerServiceDefinition;
import io.grpc.Status;
import io.grpc.netty.shaded.io.grpc.netty.NettyServerBuilder;
import io.grpc.stub.ServerCalls;
import io.grpc.stub.StreamObserver;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class GamePlayerAdminTest {
    private static final GameProto PROTO = GameProto.load();
    private static final ObjectMapper JSON = new ObjectMapper();
    private static final String KEY = "0123456789abcdef0123456789abcdef";
    private static final String PLAYER = "9007199254740995";
    private static final String REQUEST = "9007199254740993";
    private final AtomicInteger lookups = new AtomicInteger();
    private final AtomicInteger writes = new AtomicInteger();
    private final AtomicReference<DynamicMessage> received = new AtomicReference<>();
    private String sessionReply = """
            {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
             "sceneNodeId":"7","sessionId":10,"loginGeneration":"9007199254740999"}}
            """;
    private boolean dropLookup;
    private Server server;
    private Server secondServer;
    private HttpServer etcd;
    private GameServices services;

    @AfterEach
    void close() throws InterruptedException {
        if (services != null) services.close();
        if (etcd != null) etcd.stop(0);
        if (secondServer != null) secondServer.shutdownNow().awaitTermination(3, TimeUnit.SECONDS);
        if (server != null) server.shutdownNow().awaitTermination(3, TimeUnit.SECONDS);
    }

    @FunctionalInterface
    interface Handler { void invoke(DynamicMessage request, StreamObserver<DynamicMessage> observer); }

    private void reply(String method, String text, StreamObserver<DynamicMessage> observer) {
        try {
            DynamicMessage.Builder builder = DynamicMessage.newBuilder(PROTO.descriptor(method).getOutputType());
            JsonFormat.parser().merge(text, builder);
            observer.onNext(builder.build());
            observer.onCompleted();
        } catch (Exception error) { observer.onError(Status.INTERNAL.asRuntimeException()); }
    }

    private void success(DynamicMessage request, StreamObserver<DynamicMessage> observer) {
        String id = Long.toUnsignedString((long) request.getField(request.getDescriptorForType().findFieldByName("command_id")));
        reply(GamePlayerAdmin.EXECUTE, "{\"commandId\":\"" + id + "\",\"executed\":true,\"balanceAfter\":\"18446744073709551615\"}", observer);
    }

    private String start(Handler handler) throws Exception {
        ServerServiceDefinition locator = ServerServiceDefinition.builder("playerlocator.PlayerLocator")
                .addMethod(PROTO.method(GameServices.SESSION), ServerCalls.asyncUnaryCall((request, observer) -> {
                    lookups.incrementAndGet();
                    if (!dropLookup) reply(GameServices.SESSION, sessionReply, observer);
                })).build();
        ServerServiceDefinition admin = ServerServiceDefinition.builder("scene_node.SceneNodeGrpc")
                .addMethod(PROTO.method(GamePlayerAdmin.EXECUTE), ServerCalls.asyncUnaryCall((request, observer) -> {
                    writes.incrementAndGet();
                    received.set(request);
                    handler.invoke(request, observer);
                })).build();
        server = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(locator).addService(admin).build().start();
        return "127.0.0.1:" + server.getPort();
    }

    private void connect(String target, long budget) {
        services = new GameServices(PROTO, JSON, "", target, "", "", true, budget,
                "{\"7\":\"" + target + "\"}", KEY);
    }

    private ObjectNode frozen(String operation, String arguments) throws Exception {
        ObjectNode result = JSON.createObjectNode().put("operation", operation).put("requestId", REQUEST)
                .put("playerId", PLAYER).put("operator", "gm-test").put("reason", "test-only")
                .put("issuedAtMs", Long.toString(System.currentTimeMillis()));
        result.set("arguments", JSON.readTree(arguments));
        return result;
    }

    @Test
    void signatureMatchesIndependentProtobufVectorAndBindsWholePayload() throws Exception {
        // 向量由 Python 官方 protobuf 从权威描述文件序列化，再用标准 HMAC 生成。
        ObjectNode request = (ObjectNode) JSON.readTree("""
                {"commandId":"9007199254740993","playerId":"9007199254740995","operator":"gm-test",
                 "reason":"test-only","targetNodeId":7,"expectedSessionId":10,
                 "expectedLoginGeneration":"9007199254740999","issuedAtMs":"1700000000123",
                 "addCurrency":{"currencyType":2,"amount":"123456"}}
                """);
        DynamicMessage message = PROTO.request(GamePlayerAdmin.EXECUTE, request);
        DynamicMessage signed = PROTO.signPlayerCommand(GamePlayerAdmin.EXECUTE, message, KEY, 1700000000, REQUEST);
        String auth = PROTO.json(signed, JSON).path("auth").asText();
        assertEquals("gm-test|1700000000|9007199254740993|dedbaa0b050bd6dba806c474571f1adf395fc840e9470f347932fc8d44fea056", auth);
        request.put("playerId", "9007199254740996");
        assertNotEquals(auth, PROTO.json(PROTO.signPlayerCommand(GamePlayerAdmin.EXECUTE,
                PROTO.request(GamePlayerAdmin.EXECUTE, request), KEY, 1700000000, REQUEST), JSON).path("auth").asText());
    }

    @Test
    void sevenCommandsUseTrustedRouteAndKeepFrozenRequestUnchanged() throws Exception {
        connect(start(this::success), 5000);
        assertTrue(services.playerCommandsEnabled());
        Map<String, String> arguments = Map.of(
                "currency.add", "{\"currencyType\":0,\"amount\":\"9223372036854775807\"}",
                "currency.deduct", "{\"currencyType\":1,\"amount\":\"7\"}",
                "currency.block", "{\"currencyType\":2}", "currency.unblock", "{\"currencyType\":0}",
                "player.level", "{\"level\":50}", "pet.grant", "{\"petTableId\":101}",
                "item.grant", "{\"configId\":102,\"count\":3}");
        for (var entry : arguments.entrySet()) {
            ObjectNode input = frozen(entry.getKey(), entry.getValue());
            ObjectNode original = input.deepCopy();
            ObjectNode result = services.sendPlayerCommand(input);
            assertEquals(original, input);
            assertTrue(result.path("executed").asBoolean());
            assertEquals("18446744073709551615", result.path("balanceAfter").asText());
            ObjectNode wire = PROTO.json(received.get(), JSON);
            assertEquals(REQUEST, wire.path("commandId").asText());
            assertEquals(PLAYER, wire.path("playerId").asText());
            assertEquals(7, wire.path("targetNodeId").asInt());
            assertEquals(10, wire.path("expectedSessionId").asInt());
            assertEquals("9007199254740999", wire.path("expectedLoginGeneration").asText());
            assertEquals(input.path("issuedAtMs"), wire.path("issuedAtMs"));
            assertFalse(wire.has("arguments"));
            assertFalse(wire.has("operation"));
        }
        assertEquals(7, writes.get());
        assertEquals(7, lookups.get());
        assertEquals(PROTO.currencyTypes(), services.currencyTypes());
        assertEquals("CURRENCY_TYPE_GOLD", services.currencyTypes().get(0).path("label").asText());
    }

    @Test
    void rejectsInjectedOrOutOfRangeRequestsBeforeNetwork() throws Exception {
        connect(start(this::success), 5000);
        ObjectNode endpoint = frozen("currency.add", "{\"currencyType\":0,\"amount\":\"1\"}");
        endpoint.put("target", "attacker.example:443");
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(endpoint));
        ObjectNode numericId = frozen("pet.grant", "{\"petTableId\":1}").put("playerId", 9007199254740995L);
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(numericId));
        for (String invalid : new String[]{"{\"currencyType\":3,\"amount\":\"1\"}",
                "{\"currencyType\":0,\"amount\":\"-1\"}", "{\"currencyType\":0,\"amount\":\"9223372036854775808\"}",
                "{\"currencyType\":0,\"amount\":1}", "{\"currencyType\":0,\"amount\":\"1\",\"auth\":\"injected\"}"}) {
            ObjectNode input = frozen("currency.add", invalid);
            assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(input));
        }
        ObjectNode old = frozen("player.level", "{\"level\":50}").put("issuedAtMs", "1700000000123");
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(old));
        ObjectNode tooManyItems = frozen("item.grant", "{\"configId\":1,\"count\":100}");
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(tooManyItems));
        for (ObjectNode tooLarge : new ObjectNode[]{frozen("player.level", "{\"level\":2147483648}"),
                frozen("pet.grant", "{\"petTableId\":2147483648}"),
                frozen("item.grant", "{\"configId\":2147483648,\"count\":1}")}) {
            assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(tooLarge));
        }
        ObjectNode actorInjection = frozen("pet.grant", "{\"petTableId\":1}").put("operator", "gm|injected");
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(actorInjection));
        assertEquals(0, lookups.get());
        assertEquals(0, writes.get());
    }

    @Test
    void offlineWrongPlayerUnknownSceneAndMissingGenerationCannotWrite() throws Exception {
        connect(start(this::success), 5000);
        for (String session : new String[]{"{\"found\":false}",
                "{\"found\":true,\"session\":{\"playerId\":\"8\",\"state\":\"SESSION_STATE_ONLINE\"}}",
                "{\"found\":true,\"session\":{\"playerId\":\"9007199254740995\",\"state\":\"SESSION_STATE_ONLINE\",\"sceneNodeId\":\"8\",\"sessionId\":10,\"loginGeneration\":\"1\"}}",
                "{\"found\":true,\"session\":{\"playerId\":\"9007199254740995\",\"state\":\"SESSION_STATE_ONLINE\",\"sceneNodeId\":\"7\",\"sessionId\":10}}"}) {
            sessionReply = session;
            GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                    () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":50}")));
            assertFalse(error.isOutcomeUnknown());
        }
        assertEquals(0, writes.get());
    }

    @Test
    void levelBoundsAreEnforcedBeforeLookupAndSceneWrite() throws Exception {
        connect(start(this::success), 5000);
        for (int level : new int[]{0, 86, Integer.MAX_VALUE}) {
            ObjectNode input = frozen("player.level", "{\"level\":" + level + "}");
            GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                    () -> services.sendPlayerCommand(input));
            assertEquals("INVALID_ARGUMENT", error.getCode());
            assertFalse(error.isOutcomeUnknown());
        }
        assertEquals(0, lookups.get());
        assertEquals(0, writes.get());
        for (int level : new int[]{1, 85}) {
            assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":" + level + "}")).path("executed").asBoolean());
            assertEquals(level, PROTO.json(received.get(), JSON).path("setLevel").path("level").asInt());
        }
        assertEquals(2, lookups.get());
        assertEquals(2, writes.get());
    }

    @Test
    void nonzeroTipFalseExecutionAndWrongReceiptAreUnknown() throws Exception {
        connect(start((request, observer) -> reply(GamePlayerAdmin.EXECUTE, switch (writes.get()) {
            case 1 -> "{\"errorMessage\":{\"id\":1003},\"executed\":true}";
            case 2 -> "{\"commandId\":\"9007199254740993\"}";
            default -> "{\"commandId\":\"8\",\"executed\":true}";
        }, observer)), 5000);
        for (int index = 0; index < 3; index++) {
            GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                    () -> services.sendPlayerCommand(frozen("pet.grant", "{\"petTableId\":101}")));
            assertTrue(error.isOutcomeUnknown());
        }
        assertEquals(3, writes.get());
    }

    @Test
    void writeTimeoutIsUnknownWithoutRetry() throws Exception {
        connect(start((request, observer) -> { }), 1000);
        GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("pet.grant", "{\"petTableId\":101}")));
        assertEquals("DEADLINE_EXCEEDED", error.getCode());
        assertTrue(error.isOutcomeUnknown());
        assertEquals(1, writes.get());
    }

    @Test
    void locatorTimeoutNeverStartsCommand() throws Exception {
        connect(start(this::success), 1000);
        dropLookup = true;
        GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("pet.grant", "{\"petTableId\":101}")));
        assertEquals("DEADLINE_EXCEEDED", error.getCode());
        assertFalse(error.isOutcomeUnknown());
        assertEquals(0, writes.get());
    }

    @Test
    void explicitAuthorizationRefusalAndUnconfiguredServiceFailClosed() throws Exception {
        String target = start((request, observer) -> observer.onError(Status.PERMISSION_DENIED.asRuntimeException()));
        connect(target, 5000);
        GameServices.CallFailure error = assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("pet.grant", "{\"petTableId\":101}")));
        assertEquals("PERMISSION_DENIED", error.getCode());
        assertFalse(error.isOutcomeUnknown());
        services.close();
        services = new GameServices(PROTO, JSON, "", target, "", "", true, 5000);
        assertFalse(services.playerCommandsEnabled());
        assertThrows(GameServices.CallFailure.class, () -> services.sendPlayerCommand(frozen("pet.grant", "{\"petTableId\":101}")));
        assertThrows(IllegalArgumentException.class, () -> new GameServices(PROTO, JSON, "", "", "", "", true,
                5000, "{\"7\":\"example.com:443\"}", KEY));
        assertThrows(IllegalArgumentException.class, () -> new GameServices(PROTO, JSON, "", target, "", "", true,
                5000, "{}", "http://etcd.example.com:2379", KEY, 0));
    }

    private static String b64(String text) {
        return Base64.getEncoder().encodeToString(text.getBytes(StandardCharsets.UTF_8));
    }

    private String startEtcd(AtomicReference<String> bodyRef) throws Exception {
        etcd = HttpServer.create(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0), 0);
        etcd.createContext("/v3/kv/range", exchange -> {
            byte[] payload = bodyRef.get().getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, payload.length);
            try (var out = exchange.getResponseBody()) { out.write(payload); }
        });
        etcd.start();
        return "http://127.0.0.1:" + etcd.getAddress().getPort();
    }

    @Test
    void dynamicEtcdDiscoveryResolvesRotatesAndRejectsAmbiguousOrUnsafeTargets() throws Exception {
        String firstTarget = start(this::success);
        int firstPort = server.getPort();
        var secondWrites = new AtomicInteger();
        ServerServiceDefinition secondAdmin = ServerServiceDefinition.builder("scene_node.SceneNodeGrpc")
                .addMethod(PROTO.method(GamePlayerAdmin.EXECUTE), ServerCalls.asyncUnaryCall((request, observer) -> {
                    secondWrites.incrementAndGet();
                    success(request, observer);
                })).build();
        secondServer = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(secondAdmin).build().start();
        int secondPort = secondServer.getPort();

        // 包含 /allocated/ 占位键、缺 grpcEndpoint 的节点、以及正常注册的 node 7。
        String initialBody = "{\"kvs\":["
                + "{\"key\":\"" + b64("SceneNodeService.rpc/allocated/node_type/3/node_id/7") + "\",\"value\":\"" + b64("uuid-placeholder") + "\"},"
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/8") + "\",\"value\":\"" + b64("{\"nodeId\":8,\"endpoint\":{\"ip\":\"127.0.0.1\",\"port\":9000}}") + "\"},"
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7") + "\",\"value\":\"" + b64("{\"nodeId\":7,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + firstPort + "}}") + "\"}"
                + "]}";
        var etcdBody = new AtomicReference<>(initialBody);
        String etcdUrl = startEtcd(etcdBody);

        services = new GameServices(PROTO, JSON, "", firstTarget, "", "", true, 5000, "{}", etcdUrl, KEY, 0);
        assertTrue(services.playerCommandsEnabled());
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":50}")).path("executed").asBoolean());
        assertEquals(1, writes.get());
        assertEquals(0, secondWrites.get());

        // 场景节点重建并换端口后，自动切换通道到新实例。
        etcdBody.set("{\"kvs\":[{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7") + "\",\"value\":\""
                + b64("{\"nodeId\":7,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + secondPort + "}}") + "\"}]}");
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":51}")).path("executed").asBoolean());
        assertEquals(1, writes.get());
        assertEquals(1, secondWrites.get());

        // 同 nodeId 出现两个不同地址（脑裂注册）必须 fail-closed，拒绝执行。
        etcdBody.set("{\"kvs\":["
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7-a") + "\",\"value\":\"" + b64("{\"nodeId\":7,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + firstPort + "}}") + "\"},"
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7-b") + "\",\"value\":\"" + b64("{\"nodeId\":7,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + secondPort + "}}") + "\"}"
                + "]}");
        GameServices.CallFailure ambiguous = assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":52}")));
        assertEquals("SCENE_AMBIGUOUS", ambiguous.getCode());
        assertFalse(ambiguous.isOutcomeUnknown());

        // 明文模式下 etcd 返回非回环远端地址必须被安全策略拒绝。
        etcdBody.set("{\"kvs\":[{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7") + "\",\"value\":\""
                + b64("{\"nodeId\":7,\"grpcEndpoint\":{\"ip\":\"10.0.0.9\",\"port\":50001}}") + "\"}]}");
        GameServices.CallFailure unsafe = assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":53}")));
        assertEquals("SCENE_TARGET_INVALID", unsafe.getCode());
        assertFalse(unsafe.isOutcomeUnknown());
        assertEquals(1, writes.get());
        assertEquals(1, secondWrites.get());

        // 静态映射优先于 etcd：即便 etcd 处于不安全/分叉状态，静态映射的节点仍可安全命中。
        services.close();
        services = new GameServices(PROTO, JSON, "", firstTarget, "", "", true, 5000,
                "{\"7\":\"" + firstTarget + "\"}", etcdUrl, KEY, 0);
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":54}")).path("executed").asBoolean());
        assertEquals(2, writes.get());
    }

    @Test
    void multiZonePlacementResolvesSameNodeIdAcrossZonesAndRejectsHandoffOrMismatch() throws Exception {
        String firstTarget = start(this::success);
        int firstPort = server.getPort();
        var secondWrites = new AtomicInteger();
        ServerServiceDefinition secondAdmin = ServerServiceDefinition.builder("scene_node.SceneNodeGrpc")
                .addMethod(PROTO.method(GamePlayerAdmin.EXECUTE), ServerCalls.asyncUnaryCall((request, observer) -> {
                    secondWrites.incrementAndGet();
                    success(request, observer);
                })).build();
        secondServer = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(secondAdmin).build().start();
        int secondPort = secondServer.getPort();

        // Zone 1 与 Zone 2 同时存在 nodeId=7，必须按 (zoneId, nodeId) 精确区分而不误判歧义。
        String multiZoneEtcd = "{\"kvs\":["
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/1/node/7") + "\",\"value\":\""
                + b64("{\"nodeId\":7,\"zoneId\":1,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + firstPort + "}}") + "\"},"
                + "{\"key\":\"" + b64("SceneNodeService.rpc/zone/2/node/7") + "\",\"value\":\""
                + b64("{\"nodeId\":7,\"zoneId\":2,\"grpcEndpoint\":{\"ip\":\"127.0.0.1\",\"port\":" + secondPort + "}}") + "\"}"
                + "]}";
        String etcdUrl = startEtcd(new AtomicReference<>(multiZoneEtcd));
        services = new GameServices(PROTO, JSON, "", firstTarget, "", "", true, 5000, "{}", etcdUrl, KEY, 0);

        // 模拟真实生产：PlayerSession.sceneNodeId 为空，权威落点来自 placement。
        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sceneNodeId":"","sessionId":10,"loginGeneration":"9007199254740999"},
                 "placement":{"found":true,"zoneId":1,"nodeId":"7","sceneId":"100","ownerEpoch":"5",
                 "awaitingPlacement":false,"sessionId":10,"loginGeneration":"9007199254740999"}}
                """;
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":50}")).path("executed").asBoolean());
        assertEquals(1, writes.get());
        assertEquals(0, secondWrites.get());

        // 切到 Zone 2 同号 node 7，精确命中 secondServer。
        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sceneNodeId":"","sessionId":10,"loginGeneration":"9007199254740999"},
                 "placement":{"found":true,"zoneId":2,"nodeId":"7","sceneId":"200","ownerEpoch":"6",
                 "awaitingPlacement":false,"sessionId":10,"loginGeneration":"9007199254740999"}}
                """;
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":51}")).path("executed").asBoolean());
        assertEquals(1, writes.get());
        assertEquals(1, secondWrites.get());

        // 未入场 / 跨区交接等待落点 / 会话代次未对齐均 fail-closed 且不触发写 RPC。
        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sessionId":10,"loginGeneration":"9007199254740999"},"placement":{"found":false}}
                """;
        assertEquals("PLAYER_NOT_IN_SCENE", assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":52}"))).getCode());

        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sessionId":10,"loginGeneration":"9007199254740999"},
                 "placement":{"found":true,"zoneId":2,"nodeId":"","ownerEpoch":"7","awaitingPlacement":true}}
                """;
        assertEquals("PLAYER_HANDOFF", assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":52}"))).getCode());

        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sessionId":10,"loginGeneration":"9007199254740999"},
                 "placement":{"found":true,"zoneId":2,"nodeId":"7","sceneId":"200","ownerEpoch":"8",
                 "sessionId":10,"loginGeneration":"9007199254740998"}}
                """;
        assertEquals("SESSION_MISMATCH", assertThrows(GameServices.CallFailure.class,
                () -> services.sendPlayerCommand(frozen("player.level", "{\"level\":52}"))).getCode());

        assertEquals(1, writes.get());
        assertEquals(1, secondWrites.get());

        // 静态多区映射 {"2/7": ...} 支持精确覆盖指定区节点。
        services.close();
        services = new GameServices(PROTO, JSON, "", firstTarget, "", "", true, 5000,
                "{\"2/7\":\"127.0.0.1:" + secondPort + "\"}", KEY);
        sessionReply = """
                {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                 "sessionId":10,"loginGeneration":"9007199254740999"},
                 "placement":{"found":true,"zoneId":2,"nodeId":"7","sceneId":"200","ownerEpoch":"9",
                 "sessionId":10,"loginGeneration":"9007199254740999"}}
                """;
        assertTrue(services.sendPlayerCommand(frozen("player.level", "{\"level\":53}")).path("executed").asBoolean());
        assertEquals(2, secondWrites.get());
    }
}
