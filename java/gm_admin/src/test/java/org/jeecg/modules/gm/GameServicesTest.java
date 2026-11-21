package org.jeecg.modules.gm;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.util.JsonFormat;
import io.grpc.Server;
import io.grpc.ServerServiceDefinition;
import io.grpc.Status;
import io.grpc.netty.shaded.io.grpc.netty.NettyServerBuilder;
import io.grpc.stub.ServerCalls;
import io.grpc.stub.StreamObserver;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class GameServicesTest {
    private static final GameProto PROTO = GameProto.load();
    private static final ObjectMapper MAPPER = new ObjectMapper();
    private static final String SECRET = "0123456789abcdef0123456789abcdef";
    private static final String PLAYER = "9007199254740995";
    private Server server;
    private GameServices services;
    private final AtomicInteger sends = new AtomicInteger();
    private final AtomicInteger reads = new AtomicInteger();
    private final AtomicReference<DynamicMessage> received = new AtomicReference<>();

    @AfterEach
    void close() throws InterruptedException {
        if (services != null) services.close();
        if (server != null) server.shutdownNow().awaitTermination(3, TimeUnit.SECONDS);
    }

    private ObjectNode frozen() throws Exception {
        return (ObjectNode) MAPPER.readTree("""
                {"senderKind":1,"requestId":"9007199254740993","recipientPlayerId":"9007199254740995",
                 "title":"GM test","body":"mail grant","expireMs":"1900000000000",
                 "attachments":{"currencies":[{"currencyType":0,"amount":"100000"}],
                                "items":[{"configId":101,"count":2}]}}
                """);
    }

    private DynamicMessage response(String method, String json) throws Exception {
        DynamicMessage.Builder builder = DynamicMessage.newBuilder(PROTO.descriptor(method).getOutputType());
        JsonFormat.parser().merge(json, builder);
        return builder.build();
    }

    private void reply(String method, String json, StreamObserver<DynamicMessage> observer) {
        try {
            observer.onNext(response(method, json));
            observer.onCompleted();
        } catch (Exception error) {
            observer.onError(Status.INTERNAL.asRuntimeException());
        }
    }

    @FunctionalInterface
    interface MailHandler { void invoke(DynamicMessage request, StreamObserver<DynamicMessage> observer); }

    private String start(MailHandler handler) throws Exception {
        ServerServiceDefinition data = ServerServiceDefinition.builder("data_service.DataService")
                .addMethod(PROTO.method(GameServices.DISPLAY), ServerCalls.asyncUnaryCall((request, observer) -> {
                    reads.incrementAndGet();
                    reply(GameServices.DISPLAY, """
                        {"players":{"9007199254740995":{"name":"测试角色","level":35,"classId":2,
                        "gender":1,"appearanceId":"hero"}}}
                        """, observer);
                }))
                .addMethod(PROTO.method(GameServices.HOME), ServerCalls.asyncUnaryCall((request, observer) -> {
                    reads.incrementAndGet();
                    reply(GameServices.HOME, "{\"homeZoneId\":7}", observer);
                })).build();
        ServerServiceDefinition locator = ServerServiceDefinition.builder("playerlocator.PlayerLocator")
                .addMethod(PROTO.method(GameServices.SESSION), ServerCalls.asyncUnaryCall((request, observer) -> {
                    reads.incrementAndGet();
                    reply(GameServices.SESSION, """
                        {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                        "sceneNodeId":"scene-1","sceneId":"9007199254740997","lastActiveTs":"1700000000123",
                        "tokenId":"must-not-return","account":"must-not-return","requestId":"must-not-return",
                        "sessionId":10,"gateInstanceId":"must-not-return"}}
                        """, observer);
                })).build();
        ServerServiceDefinition mail = ServerServiceDefinition.builder("mailpb.MailAdmin")
                .addMethod(PROTO.method(GameServices.SEND_MAIL), ServerCalls.asyncUnaryCall((request, observer) -> {
                    sends.incrementAndGet();
                    received.set(request);
                    handler.invoke(request, observer);
                })).build();
        server = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(data).addService(locator).addService(mail).build().start();
        return "127.0.0.1:" + server.getPort();
    }

    private void connect(String target, long budget) {
        services = new GameServices(PROTO, MAPPER, target, target, target, SECRET, true, budget);
    }

    @Test
    void signatureMatchesAuthoritativeGoServiceauthVector() throws Exception {
        // 固定向量由现役 go/shared/serviceauth.Sign 生成，不由 Java 待测实现推导。
        DynamicMessage request = PROTO.request(GameServices.SEND_MAIL, frozen());
        DynamicMessage signed = PROTO.sign(GameServices.SEND_MAIL, request, "admin", SECRET, 1700000000123L);
        ObjectNode json = PROTO.json(signed, MAPPER);
        assertEquals("78833e9523509cc5d2dfb5351e46777af7fce9a05951a2dede4464d7ff5f2c7a",
                json.path("auth").path("signatureHex").asText());
        assertEquals("1700000000123", json.path("auth").path("timestampMs").asText());
        assertEquals("9007199254740993", json.path("requestId").asText());
    }

    @Test
    void realGrpcRoundtripPreservesIdsAndDoesNotExposeSessionSecrets() throws Exception {
        String target = start((request, observer) -> reply(GameServices.SEND_MAIL,
                "{\"mailId\":\"9007199254740999\",\"duplicated\":true}", observer));
        connect(target, 5000);
        ObjectNode view = services.player(PLAYER);
        assertTrue(view.path("found").asBoolean());
        assertEquals("测试角色", view.path("display").path("name").asText());
        assertEquals("9007199254740997", view.path("session").path("sceneId").asText());
        assertEquals(4, view.path("session").size());
        assertFalse(view.toString().contains("must-not-return"));
        assertEquals(3, reads.get());
        ObjectNode frozen = frozen();
        ObjectNode original = frozen.deepCopy();
        ObjectNode reply = services.sendMail(frozen);
        assertEquals(original, frozen, "发送不能修改已冻结的业务请求");
        assertEquals("9007199254740999", reply.path("mailId").asText());
        assertTrue(reply.path("duplicated").asBoolean());
        ObjectNode wire = PROTO.json(received.get(), MAPPER);
        assertEquals(1, wire.path("senderKind").asInt());
        assertEquals("admin", wire.path("auth").path("caller").asText());
        assertEquals(1, sends.get());
    }

    @Test
    void refusesInvalidIdsAndUnfrozenOrInjectedMailBeforeNetwork() throws Exception {
        String target = start((request, observer) -> observer.onError(Status.INTERNAL.asRuntimeException()));
        connect(target, 5000);
        for (String invalid : new String[]{"0", "-1", "01", "1.5", "1e6", "18446744073709551616", "x", ""}) {
            assertEquals("INVALID_ARGUMENT", assertThrows(GameServices.CallFailure.class,
                    () -> services.player(invalid)).getCode());
        }
        ObjectNode numericId = frozen();
        numericId.put("requestId", 9007199254740993L);
        assertThrows(GameServices.CallFailure.class, () -> services.sendMail(numericId));
        ObjectNode authInjection = frozen();
        authInjection.putObject("auth").put("caller", "rank");
        assertThrows(GameServices.CallFailure.class, () -> services.sendMail(authInjection));
        ObjectNode wrongSender = frozen();
        wrongSender.put("senderKind", 3);
        assertThrows(GameServices.CallFailure.class, () -> services.sendMail(wrongSender));
        ObjectNode bulk = frozen();
        bulk.put("campaignId", "12");
        assertThrows(GameServices.CallFailure.class, () -> services.sendMail(bulk));
        assertEquals(0, reads.get());
        assertEquals(0, sends.get());
    }

    @Test
    void nonzeroTipAndMissingReceiptNeverBecomeSuccess() throws Exception {
        String target = start((request, observer) -> reply(GameServices.SEND_MAIL,
                sends.get() == 1 ? "{\"errorMessage\":{\"id\":1003}}" : "{}", observer));
        connect(target, 5000);
        GameServices.CallFailure fault = assertThrows(GameServices.CallFailure.class, () -> services.sendMail(frozen()));
        assertEquals("SERVER_TIP", fault.getCode());
        assertEquals(1003, fault.getTipId());
        assertTrue(fault.isOutcomeUnknown());
        GameServices.CallFailure missing = assertThrows(GameServices.CallFailure.class, () -> services.sendMail(frozen()));
        assertEquals("INVALID_RESPONSE", missing.getCode());
        assertTrue(missing.isOutcomeUnknown());
        assertEquals(2, sends.get());
    }

    @Test
    void deadlineMarksUnknownAndDoesNotRetry() throws Exception {
        String target = start((request, observer) -> { /* 模拟收件后没有回包，不能推断没有提交。 */ });
        connect(target, 1000);
        long started = System.nanoTime();
        GameServices.CallFailure failure = assertThrows(GameServices.CallFailure.class, () -> services.sendMail(frozen()));
        assertEquals("DEADLINE_EXCEEDED", failure.getCode());
        assertTrue(failure.isOutcomeUnknown());
        assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - started) < 3000);
        assertEquals(1, sends.get());
    }

    @Test
    void explicitAuthRejectionIsNotReportedAsDelivery() throws Exception {
        String target = start((request, observer) -> observer.onError(Status.UNAUTHENTICATED.asRuntimeException()));
        connect(target, 5000);
        GameServices.CallFailure failure = assertThrows(GameServices.CallFailure.class, () -> services.sendMail(frozen()));
        assertEquals("UNAUTHENTICATED", failure.getCode());
        assertFalse(failure.isOutcomeUnknown());
    }

    @Test
    void unconfiguredMailAndRemotePlaintextFailClosed() throws Exception {
        services = new GameServices(PROTO, MAPPER, "", "", "", "", false, 5000);
        assertFalse(services.mailEnabled());
        assertTrue(services.supportsCurrency(0));
        assertTrue(services.supportsCurrency(1));
        assertTrue(services.supportsCurrency(2));
        assertFalse(services.supportsCurrency(3));
        assertEquals("NOT_CONFIGURED", assertThrows(GameServices.CallFailure.class,
                () -> services.sendMail(frozen())).getCode());
        services.close();
        services = new GameServices(PROTO, MAPPER, "", "", "127.0.0.1:50900", " ".repeat(40), true, 5000);
        assertFalse(services.mailEnabled());
        assertThrows(IllegalArgumentException.class, () -> new GameServices(PROTO, MAPPER,
                "example.com:443", "", "", SECRET, true, 5000));
    }

    @Test
    void playerQueryUsesAuthoritativePlacementWhenPresent() throws Exception {
        var sawIncludePlacement = new AtomicReference<Boolean>();
        ServerServiceDefinition data = ServerServiceDefinition.builder("data_service.DataService")
                .addMethod(PROTO.method(GameServices.DISPLAY), ServerCalls.asyncUnaryCall((request, observer) ->
                        reply(GameServices.DISPLAY, "{\"players\":{\"9007199254740995\":{\"name\":\"测试角色\",\"level\":35}}}", observer)))
                .addMethod(PROTO.method(GameServices.HOME), ServerCalls.asyncUnaryCall((request, observer) ->
                        reply(GameServices.HOME, "{\"homeZoneId\":1}", observer)))
                .build();
        ServerServiceDefinition locator = ServerServiceDefinition.builder("playerlocator.PlayerLocator")
                .addMethod(PROTO.method(GameServices.SESSION), ServerCalls.asyncUnaryCall((request, observer) -> {
                    sawIncludePlacement.set((Boolean) request.getField(request.getDescriptorForType().findFieldByName("include_placement")));
                    reply(GameServices.SESSION, """
                        {"found":true,"session":{"playerId":"9007199254740995","state":"SESSION_STATE_ONLINE",
                        "sceneNodeId":"","sceneId":"0","lastActiveTs":"1700000000123"},
                        "placement":{"found":true,"zoneId":2,"nodeId":"7","sceneId":"9007199254740998",
                        "ownerEpoch":"9","awaitingPlacement":false}}
                        """, observer);
                })).build();
        server = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(data).addService(locator).build().start();
        String target = "127.0.0.1:" + server.getPort();
        connect(target, 5000);
        ObjectNode view = services.player(PLAYER);
        assertEquals(Boolean.TRUE, sawIncludePlacement.get());
        assertEquals(2, view.path("session").path("sceneZoneId").asInt());
        assertEquals("7", view.path("session").path("sceneNodeId").asText());
        assertEquals("9007199254740998", view.path("session").path("sceneId").asText());
    }

    @Test
    void systemMailAndThreeStageCampaignSignAndVerifyEveryStep() throws Exception {
        var stageCalls = new java.util.ArrayList<String>();
        ServerServiceDefinition mail = ServerServiceDefinition.builder("mailpb.MailAdmin")
                .addMethod(PROTO.method(GameServices.SEND_SYSTEM_MAIL), ServerCalls.asyncUnaryCall((request, observer) -> {
                    stageCalls.add("create");
                    reply(GameServices.SEND_SYSTEM_MAIL, "{\"mailId\":\"9007199254741111\"}", observer);
                }))
                .addMethod(PROTO.method(GameServices.IMPORT_RECIPIENTS), ServerCalls.asyncUnaryCall((request, observer) -> {
                    stageCalls.add("import");
                    int count = request.getRepeatedFieldCount(request.getDescriptorForType().findFieldByName("player_ids"));
                    reply(GameServices.IMPORT_RECIPIENTS, "{\"importedCount\":\"" + count + "\"}", observer);
                }))
                .addMethod(PROTO.method(GameServices.SEAL_CAMPAIGN), ServerCalls.asyncUnaryCall((request, observer) -> {
                    stageCalls.add("seal");
                    reply(GameServices.SEAL_CAMPAIGN, "{\"mailId\":\"9007199254741111\"}", observer);
                }))
                .build();
        server = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(mail).build().start();
        String target = "127.0.0.1:" + server.getPort();
        connect(target, 5000);

        // 纯文本全服系统邮件放行，带附件时拒绝（须走活动名单流程）。
        ObjectNode notice = (ObjectNode) MAPPER.readTree("""
                {"senderKind":1,"requestId":"9007199254740991","title":"停服维护公告","body":"今晚维护","expireMs":"1900000000000"}
                """);
        assertEquals("9007199254741111", services.sendSystemMail(notice).path("mailId").asText());
        ObjectNode badNotice = notice.deepCopy();
        badNotice.putObject("attachments").putArray("currencies").addObject().put("currencyType", 0).put("amount", "10");
        assertEquals("INVALID_ARGUMENT", assertThrows(GameServices.CallFailure.class, () -> services.sendSystemMail(badNotice)).getCode());

        // 三段式活动邮件：Create -> Import -> Seal。
        var plan = MailCampaignManifest.build(java.util.List.of("1001", "1002", "9007199254740995"));
        ObjectNode campaign = MAPPER.createObjectNode();
        ObjectNode mailReq = campaign.putObject("mail")
                .put("senderKind", 1).put("requestId", "9007199254740992")
                .put("title", "全服补偿").put("body", "补偿附件").put("expireMs", "1900000000000")
                .put("campaignId", "9007199254740992").put("manifestGeneration", "1")
                .put("expectedRecipientCount", Integer.toString(plan.recipientCount()))
                .put("expectedShardCount", plan.shards().size())
                .put("manifestHash", plan.manifestHash());
        mailReq.putObject("attachments").putArray("currencies").addObject().put("currencyType", 1).put("amount", "500");
        var shardsJson = campaign.putArray("shards");
        for (var shard : plan.shards()) {
            var arr = shardsJson.addArray();
            shard.forEach(arr::add);
        }
        stageCalls.clear();
        ObjectNode sealed = services.sendMailCampaign(campaign);
        assertEquals(java.util.List.of("create", "import", "seal"), stageCalls);
        assertTrue(sealed.path("sealed").asBoolean());
        assertEquals(3, sealed.path("importedRecipients").asInt());
        assertEquals("9007199254741111", sealed.path("mailId").asText());

        // 篡改摘要后在发包前即被拦截。
        ObjectNode tampered = campaign.deepCopy();
        ((ObjectNode) tampered.get("mail")).put("manifestHash", "0".repeat(64));
        assertEquals("INVALID_ARGUMENT", assertThrows(GameServices.CallFailure.class, () -> services.sendMailCampaign(tampered)).getCode());
    }
}
