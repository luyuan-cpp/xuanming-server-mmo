package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import io.grpc.CallOptions;
import io.grpc.Deadline;
import io.grpc.ManagedChannel;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.ClientCalls;
import jakarta.annotation.PreDestroy;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * 交易人工处置(trade.TradeAdmin)的客户端(docs/design/admin-identity-and-dual-control.md §5.6)。
 *
 * <p>调用方证明用 ServiceCallAuth、调用方 ops-commerce,密钥与 trade 的 Commerce.ManualResolutionSecretEnv 同值,只挂给后台。
 * EXECUTE 带上流水生成的批准记录,它在请求体里,被签名覆盖;trade 侧用 shared/dualcontrol 再核一遍,并按批准号幂等。
 */
@Component
public class GameTradeAdmin {
    static final String LIST = "trade.TradeAdmin/ListManualContracts";
    static final String RESOLVE = "trade.TradeAdmin/ResolveManualContract";
    /** trade 唯一接受的人工处置调用方(go/trade/internal/config.ManualResolutionCaller)。 */
    static final String CALLER = "ops-commerce";
    private static final Set<String> FROZEN_FIELDS = Set.of("commerceId", "action", "expectVersion", "expectTarget", "operator", "reason",
            "parts", "party", "toPlayerId", "linkedCommerceId", "linkedVersion");

    private final GameProto proto;
    private final ObjectMapper mapper;
    private final ManagedChannel trade;
    private final String secret;
    private final long budgetMillis;

    @Autowired
    public GameTradeAdmin(GameServices services, ObjectMapper mapper, @Value("${gm.game.trade-target:}") String target,
            @Value("${gm.game.trade-admin-secret:}") String secret, @Value("${gm.game.plaintext:false}") boolean plaintext) {
        this(services.proto(), mapper, GameServices.createChannel(target, plaintext), secret, 5000);
    }

    GameTradeAdmin(GameProto proto, ObjectMapper mapper, ManagedChannel trade, String secret, long budgetMillis) {
        this.proto = proto; this.mapper = mapper; this.trade = trade;
        this.secret = secret == null ? "" : secret.strip();
        this.budgetMillis = budgetMillis;
    }

    public boolean enabled() { return trade != null && secret.getBytes(StandardCharsets.UTF_8).length >= 32; }

    /** 转人工的合同,一页最多 limit 条;游标为两个十进制字符串。 */
    public ObjectNode list(String afterUpdatedMs, String afterCommerceId, int limit) {
        ObjectNode request = mapper.createObjectNode().put("afterUpdatedMs", afterUpdatedMs).put("afterCommerceId", afterCommerceId).put("limit", limit);
        return json(call(LIST, request, false));
    }

    /** SHOW:锁外读合同、腿、两方归属,逐个动作试算;不写。 */
    public ObjectNode show(String commerceId) {
        ObjectNode request = mapper.createObjectNode().put("commerceId", commerceId).put("mode", "MANUAL_CONTRACT_MODE_SHOW");
        return json(call(RESOLVE, request, false));
    }

    /** DRY_RUN:与执行同一锁序、同一判据算计划;不写。frozen 是流水里冻结的业务字段(含 operator、reason)。 */
    public ObjectNode dryRun(ObjectNode frozen) {
        return json(call(RESOLVE, resolveRequest(frozen, "MANUAL_CONTRACT_MODE_DRY_RUN", null), false));
    }

    /**
     * EXECUTE,给流水的放行用。trade 回 OK(含同一批准号的重放)返回回包;确定的拒绝抛不可重试的 CallFailure(流水记 REJECTED);
     * 读库失败(RETRY)、提交结果未知或连接问题抛可重试的 CallFailure(流水记 UNKNOWN,用原批准号重试由 trade 判重)。
     */
    public ObjectNode execute(ObjectNode frozen, ObjectNode approval) {
        ObjectNode reply = json(call(RESOLVE, resolveRequest(frozen, "MANUAL_CONTRACT_MODE_EXECUTE", approval), true));
        String verdict = reply.path("verdict").asText("");
        switch (verdict) {
            case "MANUAL_CONTRACT_VERDICT_OK":
                return reply;
            case "MANUAL_CONTRACT_VERDICT_RETRY":
                throw new GameServices.CallFailure("TRADE_RETRY", "交易服务本次读库失败，未改动；可用原命令重试", true, 0);
            case "MANUAL_CONTRACT_VERDICT_NOT_FOUND":
            case "MANUAL_CONTRACT_VERDICT_REFUSED":
            case "MANUAL_CONTRACT_VERDICT_VERSION_CHANGED":
                throw new GameServices.CallFailure(verdict.substring("MANUAL_CONTRACT_VERDICT_".length()),
                        "交易服务拒绝执行：" + reply.path("refusal").asText(""), false, 0);
            default:
                throw new GameServices.CallFailure("INVALID_RESPONSE", "交易服务回包无法判定，请刷新核对", true, 0);
        }
    }

    private ObjectNode resolveRequest(ObjectNode frozen, String mode, ObjectNode approval) {
        ObjectNode request = mapper.createObjectNode();
        frozen.fields().forEachRemaining(field -> {
            if (FROZEN_FIELDS.contains(field.getKey())) request.set(field.getKey(), field.getValue().deepCopy());
        });
        request.put("mode", mode);
        if (approval != null) request.set("approval", approval.deepCopy());
        return request;
    }

    private DynamicMessage call(String method, ObjectNode json, boolean writing) {
        if (!enabled()) throw new GameServices.CallFailure("NOT_CONFIGURED", "交易管理通道未配置", false, 0);
        DynamicMessage request;
        try { request = proto.request(method, json); }
        catch (IOException | IllegalArgumentException invalid) {
            throw new GameServices.CallFailure("INVALID_ARGUMENT", "交易人工处置请求不符合游戏协议", false, 0);
        }
        request = proto.sign(method, request, CALLER, secret, System.currentTimeMillis());
        try {
            return ClientCalls.blockingUnaryCall(trade, proto.method(method),
                    CallOptions.DEFAULT.withDeadline(Deadline.after(budgetMillis, TimeUnit.MILLISECONDS)), request);
        } catch (StatusRuntimeException error) {
            Status.Code code = error.getStatus().getCode();
            boolean refused = code == Status.Code.UNAUTHENTICATED || code == Status.Code.PERMISSION_DENIED
                    || code == Status.Code.INVALID_ARGUMENT || code == Status.Code.UNIMPLEMENTED;
            // EXECUTE 遇到 UNAVAILABLE(trade 报提交结果未知)、超时或断连都不能当成没做:记成未知,用原批准号重试由 trade 判重。
            throw new GameServices.CallFailure(code.name(), writing && !refused ? "交易处置结果未知，请用原命令重试或刷新核对"
                    : "交易服务调用未完成：" + code.name(), writing && !refused, 0);
        }
    }

    private ObjectNode json(DynamicMessage reply) {
        try {
            return proto.json(reply, mapper);
        } catch (IOException invalid) {
            throw new GameServices.CallFailure("INVALID_RESPONSE", "交易服务回包无法解析", true, 0);
        }
    }

    @PreDestroy
    public void close() {
        if (trade == null) return;
        trade.shutdownNow();
        try { trade.awaitTermination(2, TimeUnit.SECONDS); }
        catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); }
    }
}
