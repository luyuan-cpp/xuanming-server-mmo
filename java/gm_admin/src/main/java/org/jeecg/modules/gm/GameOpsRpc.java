package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import io.grpc.CallOptions;
import io.grpc.Channel;
import io.grpc.ClientInterceptors;
import io.grpc.Deadline;
import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.ClientCalls;
import io.grpc.stub.MetadataUtils;
import jakarta.annotation.PreDestroy;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * 运营处置的 gRPC 下游(docs/design/admin-identity-and-dual-control.md §8 批 5):账号类(login)、回收(data_service)、
 * 收件箱处置(player_inbox)、榜单删除(rank)、滞留阵容终结(match)。
 *
 * <p>调用方证明两种:login / data_service / player_inbox 的管理入口认共享口令(metadata x-admin-token),
 * rank / match 认 ServiceCallAuth(rank 的调用方 admin、match 的调用方 ops-formation)。口令与密钥只挂给后台工作负载
 * (设计 §6 第 3 条:拿到它的人能伪造批准记录,和 HMAC 密钥同一个信任边界)。批准记录放在请求体里,下游用
 * go/shared/dualcontrol 复查;带请求号的命令,请求号就是批准号。
 */
@Component
public class GameOpsRpc {
    static final String TOKEN_HEADER = "x-admin-token";
    /** 确定的拒绝:下游没有执行。其余状态码(UNAVAILABLE、DEADLINE_EXCEEDED、INTERNAL、UNKNOWN、ABORTED)写命令记成结果未知。 */
    private static final Set<Status.Code> DEFINITE = Set.of(Status.Code.UNAUTHENTICATED, Status.Code.PERMISSION_DENIED,
            Status.Code.INVALID_ARGUMENT, Status.Code.UNIMPLEMENTED, Status.Code.FAILED_PRECONDITION, Status.Code.NOT_FOUND,
            Status.Code.ALREADY_EXISTS, Status.Code.OUT_OF_RANGE);

    /** 一个下游:连接与调用方证明(token 不为空走口令;caller 不为空走 ServiceCallAuth)。 */
    record Target(ManagedChannel channel, String token, String caller, String secret) {
        boolean enabled() {
            if (channel == null) return false;
            return caller == null ? token.getBytes(StandardCharsets.UTF_8).length >= 16 : secret.getBytes(StandardCharsets.UTF_8).length >= 32;
        }
    }

    private final GameProto proto;
    private final ObjectMapper mapper;
    private final Map<String, Target> targets;
    private final long budgetMillis;

    @Autowired
    public GameOpsRpc(GameServices services, ObjectMapper mapper, @Value("${gm.game.plaintext:false}") boolean plaintext,
            @Value("${gm.game.login-target:}") String loginTarget, @Value("${gm.game.login-admin-token:}") String loginToken,
            @Value("${gm.game.data-target:}") String dataTarget, @Value("${gm.game.data-admin-token:}") String dataToken,
            @Value("${gm.game.inbox-target:}") String inboxTarget, @Value("${gm.game.inbox-admin-token:}") String inboxToken,
            @Value("${gm.game.rank-target:}") String rankTarget, @Value("${gm.game.rank-admin-secret:}") String rankSecret,
            @Value("${gm.game.match-target:}") String matchTarget, @Value("${gm.game.match-formation-secret:}") String matchSecret) {
        this(services.proto(), mapper, Map.of(
                "account", new Target(GameServices.createChannel(loginTarget, plaintext), strip(loginToken), null, ""),
                "recall", new Target(GameServices.createChannel(dataTarget, plaintext), strip(dataToken), null, ""),
                "inbox", new Target(GameServices.createChannel(inboxTarget, plaintext), strip(inboxToken), null, ""),
                "rank", new Target(GameServices.createChannel(rankTarget, plaintext), "", "admin", strip(rankSecret)),
                "match", new Target(GameServices.createChannel(matchTarget, plaintext), "", "ops-formation", strip(matchSecret))), 5000);
    }

    GameOpsRpc(GameProto proto, ObjectMapper mapper, Map<String, Target> targets, long budgetMillis) {
        this.proto = proto; this.mapper = mapper; this.targets = new HashMap<>(targets); this.budgetMillis = budgetMillis;
    }

    private static String strip(String value) { return value == null ? "" : value.strip(); }

    public boolean enabled(String target) {
        Target entry = targets.get(target);
        return entry != null && entry.enabled();
    }

    /**
     * 放行一条冻结的运营处置。frozen 是 {target, command, body, retryable, requestId};spec 给出完整方法名与请求号字段。
     * approval 为 null 表示没开双人复核(本机开发),下游要显式放开才接受。
     */
    ObjectNode execute(GmOpsController.Command spec, ObjectNode frozen, ObjectNode approval) {
        ObjectNode request = ((ObjectNode) frozen.get("body")).deepCopy();
        if (spec.requestIdField() != null) request.put(spec.requestIdField(), frozen.path("requestId").asText(""));
        if (approval != null) request.set("approval", approval.deepCopy());
        return call(spec, request, true);
    }

    /** 预览(演练、试算):不进流水、不带批准记录;失败一律是确定的「没做」。 */
    ObjectNode preview(GmOpsController.Command spec, ObjectNode body) {
        return call(spec, body, false);
    }

    private ObjectNode call(GmOpsController.Command spec, ObjectNode json, boolean writing) {
        Target target = targets.get(spec.target());
        if (target == null || !target.enabled()) throw new GameServices.CallFailure("NOT_CONFIGURED", "该下游的管理通道未配置", false, 0);
        DynamicMessage request;
        try { request = proto.request(spec.method(), json); }
        catch (IOException | IllegalArgumentException invalid) {
            throw new GameServices.CallFailure("INVALID_ARGUMENT", "运营处置请求不符合游戏协议", false, 0);
        }
        Channel channel = target.channel();
        if (target.caller() != null) {
            request = proto.sign(spec.method(), request, target.caller(), target.secret(), System.currentTimeMillis());
        } else {
            Metadata headers = new Metadata();
            headers.put(Metadata.Key.of(TOKEN_HEADER, Metadata.ASCII_STRING_MARSHALLER), target.token());
            channel = ClientInterceptors.intercept(channel, MetadataUtils.newAttachHeadersInterceptor(headers));
        }
        DynamicMessage reply;
        try {
            reply = ClientCalls.blockingUnaryCall(channel, proto.method(spec.method()),
                    CallOptions.DEFAULT.withDeadline(Deadline.after(budgetMillis, TimeUnit.MILLISECONDS)), request);
        } catch (StatusRuntimeException error) {
            Status.Code code = error.getStatus().getCode();
            boolean unknown = writing && !DEFINITE.contains(code);
            throw new GameServices.CallFailure(code.name(), unknown ? "处置结果未知：" + code.name() : "下游拒绝或调用未完成：" + code.name(), unknown, 0);
        }
        ObjectNode result;
        try { result = proto.json(reply, mapper); }
        catch (IOException invalid) { throw new GameServices.CallFailure("INVALID_RESPONSE", "下游回包无法解析", writing, 0); }
        String refusal = inBandRefusal(result);
        if (writing && refusal != null) throw new GameServices.CallFailure("REFUSED", "下游拒绝执行：" + refusal, false, tipOf(result));
        return result;
    }

    /**
     * 回包里的带内拒绝:data_service 的 error_code、login / player_inbox 的 error_message(tip)、match 的 verdict。
     * 都表示下游没有改动(match 的 RETRY 也是「已回滚、未改动」),所以按确定的拒绝记 REJECTED,要做就重新发起。
     */
    static String inBandRefusal(JsonNode reply) {
        long code = reply.path("errorCode").asLong(0);
        if (code != 0) return "error_code " + code;
        long tip = reply.path("errorMessage").path("id").asLong(0);
        if (tip != 0) return "tip " + tip;
        String verdict = reply.path("verdict").asText("");
        if (!verdict.isEmpty() && !verdict.endsWith("_ELIGIBLE")) return verdict + " " + reply.path("refusal").asText("");
        return null;
    }

    private static int tipOf(JsonNode reply) {
        long tip = reply.path("errorMessage").path("id").asLong(0);
        return tip > 0 && tip <= Integer.MAX_VALUE ? (int) tip : 0;
    }

    @PreDestroy
    public void close() {
        for (Target target : targets.values()) if (target.channel() != null) target.channel().shutdownNow();
        for (Target target : targets.values()) {
            if (target.channel() == null) continue;
            try { target.channel().awaitTermination(2, TimeUnit.SECONDS); }
            catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); return; }
        }
    }
}
