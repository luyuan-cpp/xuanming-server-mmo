package org.jeecg.modules.gm;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import io.grpc.CallOptions;
import io.grpc.ManagedChannel;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.stub.ClientCalls;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.SecureRandom;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;

/** 仅由 probe_scene.py 显式选择；IT 命名不进入常规 Surefire 单测，也不产生默认跳过项。 */
class SceneRuntimeProbeIT {
    private static final GameProto PROTO = GameProto.load();
    private static final ObjectMapper JSON = new ObjectMapper();
    private static final String EXECUTE = "scene_node.SceneNodeGrpc/GmExecutePlayerCommand";
    private static final SecureRandom RANDOM = new SecureRandom();
    private final List<ManagedChannel> channels = new ArrayList<>();

    private record Reply(Status.Code code, DynamicMessage message) {}

    private ManagedChannel channel(String name, String expected) {
        assertEquals(expected, System.getenv(name), "Probe target must come from the verified runtime bridge");
        int port = Integer.parseInt(expected.substring(expected.lastIndexOf(':') + 1));
        ManagedChannel result = NettyChannelBuilder.forAddress("127.0.0.1", port).usePlaintext()
                .disableRetry().maxInboundMessageSize(1024 * 1024).build();
        channels.add(result);
        return result;
    }

    private Reply call(ManagedChannel channel, String method, DynamicMessage request) {
        try {
            return new Reply(Status.Code.OK, ClientCalls.blockingUnaryCall(channel, PROTO.method(method),
                    CallOptions.DEFAULT.withDeadlineAfter(5, TimeUnit.SECONDS), request));
        } catch (StatusRuntimeException error) {
            // 验收记录只保留状态码，不回显内部异常说明或签名信封。
            return new Reply(error.getStatus().getCode(), null);
        }
    }

    private static String positiveId() {
        long value;
        do { value = RANDOM.nextLong() & Long.MAX_VALUE; } while (value == 0);
        return Long.toString(value);
    }

    private void requireAbsent(ManagedChannel data, ManagedChannel locator, ObjectNode byId) throws Exception {
        Reply home = call(data, GameServices.HOME, PROTO.request(GameServices.HOME, byId));
        assertEquals(Status.Code.NOT_FOUND, home.code(), "Only authoritative NOT_FOUND proves no player home mapping");
        Reply session = call(locator, GameServices.SESSION, PROTO.request(GameServices.SESSION, byId));
        assertEquals(Status.Code.OK, session.code(), "Locator lookup must complete successfully");
        assertFalse(PROTO.json(session.message(), JSON).path("found").asBoolean(), "Probe player must have no current session");
    }

    @Test
    void unsignedIsDeniedAndSignedAbsentPlayerIsRejectedWithoutMutation() throws Exception {
        assertEquals("true", System.getProperty("gm.scene.liveProbe"), "Use the explicit verified probe launcher");
        assertEquals("true", System.getenv("GM_SCENE_PROBE_READY_VERIFIED"), "Scene and RPC bridge readiness must be verified first");
        assertEquals("{\"1\":\"127.0.0.1:2355\"}", System.getenv("GM_SCENE_TARGETS_JSON"));
        String secret = System.getenv("GM_SCENE_ADMIN_SECRET");
        assertNotNull(secret);
        assertTrue(secret.getBytes(java.nio.charset.StandardCharsets.UTF_8).length >= 32);
        int rejectedTip = Integer.parseInt(System.getenv("GM_SCENE_PROBE_EXPECTED_TIP"));
        assertTrue(rejectedTip > 0);
        try {
            ManagedChannel data = channel("GM_DATA_TARGET", "127.0.0.1:2356");
            ManagedChannel locator = channel("GM_LOCATOR_TARGET", "127.0.0.1:2357");
            ManagedChannel scene = channel("GM_SCENE_PROBE_TARGET", "127.0.0.1:2355");
            String player = positiveId();
            ObjectNode byId = JSON.createObjectNode().put("playerId", player);
            requireAbsent(data, locator, byId);
            String command = positiveId();
            long issued = System.currentTimeMillis();
            ObjectNode input = JSON.createObjectNode().put("commandId", command).put("playerId", player)
                    .put("operator", "gm-runtime-probe").put("reason", "Negative probe: confirmed absent player; invalid level zero")
                    .put("targetNodeId", 1).put("expectedSessionId", 1).put("expectedLoginGeneration", "1")
                    .put("issuedAtMs", Long.toString(issued));
            // 双重保护：即使目录检查后发生并发建角，既有域函数也在任何改等级前拒绝 0。
            // 不能换成一个“很小但合法”的发奖量或等级；本探针永远不验收真实玩家资产变更。
            input.putObject("setLevel").put("level", 0);
            DynamicMessage unsigned = PROTO.request(EXECUTE, input);
            Reply denied = call(scene, EXECUTE, unsigned);
            assertEquals(Status.Code.PERMISSION_DENIED, denied.code(), "Unsigned GM request must be rejected by the real Scene");
            requireAbsent(data, locator, byId);
            DynamicMessage signed = PROTO.signPlayerCommand(EXECUTE, unsigned, secret, issued / 1000, command);
            Reply business = call(scene, EXECUTE, signed);
            assertEquals(Status.Code.OK, business.code(), "Valid dedicated signature must reach business admission");
            ObjectNode response = PROTO.json(business.message(), JSON);
            assertEquals(command, response.path("commandId").asText());
            assertFalse(response.path("executed").asBoolean());
            assertEquals(rejectedTip, response.path("errorMessage").path("id").asInt(), "Absent player must hit the exact existing rejection code");
            ObjectNode evidence = JSON.createObjectNode().put("passed", true).put("utc", Instant.now().toString())
                    .put("playerId", player).put("commandId", command).put("homeStatus", "NOT_FOUND")
                    .put("locatorFound", false).put("unsignedStatus", denied.code().name())
                    .put("signedStatus", business.code().name()).put("tipId", rejectedTip).put("executed", false)
                    .put("playerMutationTested", false).put("mailAttachmentClaimTested", false)
                    .put("raceProtection", "set_level_zero_rejected_before_domain_mutation");
            Path output = Path.of(System.getenv("GM_SCENE_PROBE_EVIDENCE"));
            assertTrue(output.isAbsolute());
            Files.writeString(output, JSON.writerWithDefaultPrettyPrinter().writeValueAsString(evidence) + "\n");
        } finally {
            for (ManagedChannel channel : channels) channel.shutdownNow().awaitTermination(3, TimeUnit.SECONDS);
        }
    }
}
