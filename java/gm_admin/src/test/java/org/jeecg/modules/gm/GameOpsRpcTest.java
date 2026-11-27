package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.util.JsonFormat;
import io.grpc.Contexts;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ServerInterceptors;
import io.grpc.ServerServiceDefinition;
import io.grpc.Status;
import io.grpc.netty.shaded.io.grpc.netty.NettyServerBuilder;
import io.grpc.stub.ServerCalls;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;

/** 运营处置的 gRPC 下游:两种调用方证明、请求号就是批准号、带内拒绝不算未知、断连算未知。 */
class GameOpsRpcTest {
    private static final GameProto PROTO = GameProto.load();
    private static final ObjectMapper MAPPER = new ObjectMapper();
    private static final String SECRET = "rank-admin-test-only-secret-000000000001";
    private static final String TOKEN = "inbox-admin-test-only-token-01";
    private static final String REMOVE = "rankpb.RankAdmin/RemoveRankEntry";
    private static final String VOID = "player_inbox.PlayerInboxAdmin/VoidItem";
    private final List<String> tokens = new CopyOnWriteArrayList<>();
    private final AtomicReference<DynamicMessage> removeRequest = new AtomicReference<>();
    private final AtomicReference<String> voidReply = new AtomicReference<>("{}");
    private Server server;
    private GameOpsRpc rpc;

    @AfterEach void close() throws InterruptedException {
        if (rpc != null) rpc.close();
        if (server != null) server.shutdownNow().awaitTermination(3, TimeUnit.SECONDS);
    }

    private String start() throws Exception {
        ServerServiceDefinition rank = ServerServiceDefinition.builder("rankpb.RankAdmin")
                .addMethod(PROTO.method(REMOVE), ServerCalls.asyncUnaryCall((request, observer) -> {
                    removeRequest.set(request);
                    observer.onNext(DynamicMessage.getDefaultInstance(PROTO.descriptor(REMOVE).getOutputType()));
                    observer.onCompleted();
                })).build();
        ServerServiceDefinition inbox = ServerServiceDefinition.builder("player_inbox.PlayerInboxAdmin")
                .addMethod(PROTO.method(VOID), ServerCalls.asyncUnaryCall((request, observer) -> {
                    try {
                        DynamicMessage.Builder reply = DynamicMessage.newBuilder(PROTO.descriptor(VOID).getOutputType());
                        JsonFormat.parser().merge(voidReply.get(), reply);
                        observer.onNext(reply.build());
                        observer.onCompleted();
                    } catch (Exception error) {
                        observer.onError(Status.INTERNAL.asRuntimeException());
                    }
                })).build();
        ServerInterceptor capture = new ServerInterceptor() {
            @Override public <Q, R> ServerCall.Listener<Q> interceptCall(ServerCall<Q, R> call, Metadata headers, ServerCallHandler<Q, R> next) {
                String token = headers.get(Metadata.Key.of(GameOpsRpc.TOKEN_HEADER, Metadata.ASCII_STRING_MARSHALLER));
                tokens.add(token == null ? "-" : token);
                return Contexts.interceptCall(io.grpc.Context.current(), call, headers, next);
            }
        };
        server = NettyServerBuilder.forAddress(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
                .addService(ServerInterceptors.intercept(rank, capture)).addService(ServerInterceptors.intercept(inbox, capture)).build().start();
        String target = "127.0.0.1:" + server.getPort();
        rpc = new GameOpsRpc(PROTO, MAPPER, Map.of(
                "rank", new GameOpsRpc.Target(GameServices.createChannel(target, true), "", "admin", SECRET),
                "inbox", new GameOpsRpc.Target(GameServices.createChannel(target, true), TOKEN, null, "")), 3000);
        return target;
    }

    private static ObjectNode frozen(String target, String command, ObjectNode body) {
        ObjectNode frozen = MAPPER.createObjectNode().put("target", target).put("command", command).put("retryable", true).put("requestId", "910000001");
        frozen.set("body", body);
        return frozen;
    }

    private static ObjectNode approval() {
        return MAPPER.createObjectNode().put("approvalId", "910000001").put("proposer", "alice").put("approver", "bob").put("approvedAtMs", "1800000000000");
    }

    @Test void serviceAuthTargetsAreSignedAndCarryTheApprovalAsTheRequestId() throws Exception {
        start();
        ObjectNode body = MAPPER.createObjectNode().put("boardId", "1").put("generation", "2").put("entityId", "3");
        rpc.execute(GmOpsController.COMMANDS.get("rank/RemoveRankEntry"), frozen("rank", "RemoveRankEntry", body), approval());
        ObjectNode sent = PROTO.json(removeRequest.get(), MAPPER);
        assertEquals("910000001", sent.path("requestId").asText(), "请求号就是批准号");
        assertEquals("910000001", sent.path("approval").path("approvalId").asText());
        assertEquals("bob", sent.path("approval").path("approver").asText());
        assertEquals("admin", sent.path("auth").path("caller").asText());
        assertTrue(sent.path("auth").path("signatureHex").asText().matches("[0-9a-f]{64}"));
        assertEquals(List.of("-"), tokens, "ServiceCallAuth 的下游不带口令");
    }

    @Test void tokenTargetsSendTheTokenAndInBandRefusalsAreDefinite() throws Exception {
        start();
        ObjectNode body = MAPPER.createObjectNode().put("playerId", "42").put("domain", "PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION").put("seq", "7").put("reason", "PLAYER_INBOX_VOID_CANCELLED").put("operator", "alice").put("note", "工单");
        voidReply.set("{\"errorMessage\":{\"id\":5}}");
        GameServices.CallFailure refused = assertThrows(GameServices.CallFailure.class,
                () -> rpc.execute(GmOpsController.COMMANDS.get("inbox/VoidItem"), frozen("inbox", "VoidItem", body), approval()));
        assertFalse(refused.isOutcomeUnknown(), "下游带内拒绝表示没有改动");
        assertEquals(5, refused.getTipId());
        assertEquals(List.of(TOKEN), tokens);
        voidReply.set("{\"state\":\"PLAYER_INBOX_ITEM_STATE_VOIDED\"}");
        assertEquals("PLAYER_INBOX_ITEM_STATE_VOIDED",
                rpc.execute(GmOpsController.COMMANDS.get("inbox/VoidItem"), frozen("inbox", "VoidItem", body), approval()).path("state").asText());
    }

    @Test void anUnreachableTargetIsAnUnknownOutcomeForWritesAndUnconfiguredTargetsAreRefused() throws Exception {
        rpc = new GameOpsRpc(PROTO, MAPPER, Map.of(
                "rank", new GameOpsRpc.Target(GameServices.createChannel("127.0.0.1:1", true), "", "admin", SECRET),
                "inbox", new GameOpsRpc.Target(null, TOKEN, null, "")), 2000);
        ObjectNode body = MAPPER.createObjectNode().put("boardId", "1").put("generation", "2").put("entityId", "3");
        assertTrue(assertThrows(GameServices.CallFailure.class,
                () -> rpc.execute(GmOpsController.COMMANDS.get("rank/RemoveRankEntry"), frozen("rank", "RemoveRankEntry", body), approval())).isOutcomeUnknown());
        assertFalse(rpc.enabled("inbox"));
        assertFalse(rpc.enabled("match"));
        assertFalse(assertThrows(GameServices.CallFailure.class,
                () -> rpc.preview(GmOpsController.COMMANDS.get("inbox/VoidItem"), body)).isOutcomeUnknown());
    }

    @Test void inBandRefusalsCoverEveryDownstreamShape() {
        assertNull(GameOpsRpc.inBandRefusal(MAPPER.createObjectNode()));
        assertNotNull(GameOpsRpc.inBandRefusal(MAPPER.createObjectNode().put("errorCode", 3)));
        assertNull(GameOpsRpc.inBandRefusal(MAPPER.createObjectNode().put("verdict", "STRANDED_FORMATION_VERDICT_ELIGIBLE")));
        assertNotNull(GameOpsRpc.inBandRefusal(MAPPER.createObjectNode().put("verdict", "STRANDED_FORMATION_VERDICT_RESOLVED")));
        assertNotNull(GameOpsRpc.inBandRefusal(MAPPER.createObjectNode().put("verdict", "STRANDED_FORMATION_VERDICT_RETRY")));
    }
}
