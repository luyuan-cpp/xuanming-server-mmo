package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.google.protobuf.DynamicMessage;
import io.grpc.CallOptions;
import io.grpc.Deadline;
import io.grpc.ManagedChannel;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.stub.ClientCalls;
import java.io.IOException;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;

/** 七项明确的在线玩家管理操作；节点地址仅由部署配置提供，不接受玩家会话或任意 RPC。 */
final class GamePlayerAdmin {
    static final String EXECUTE = "scene_node.SceneNodeGrpc/GmExecutePlayerCommand";
    // 对齐 cpp/libs/services/scene/player/system/player_level_rules.h 的可写等级范围。
    static final int MAX_PLAYER_LEVEL = 85;
    private static final BigInteger UINT64_MAX = new BigInteger("18446744073709551615");
    private static final Set<String> FIELDS = Set.of("operation", "arguments", "requestId", "playerId",
            "operator", "reason", "issuedAtMs");
    private static final Map<String, String> COMMANDS = Map.of(
            "currency.add", "addCurrency", "currency.deduct", "deductCurrency",
            "currency.block", "blockCurrency", "currency.unblock", "unblockCurrency",
            "player.level", "setLevel", "pet.grant", "grantPet", "item.grant", "grantItem");
    private final GameProto proto;
    private final ObjectMapper mapper;
    private final ManagedChannel locator;
    private final Map<String, ManagedChannel> scenes;
    private final String secret;
    private final long budgetMillis;

    GamePlayerAdmin(GameProto proto, ObjectMapper mapper, ManagedChannel locator, String targetsJson,
            String secret, long budgetMillis, Function<String, ManagedChannel> channels) {
        this.proto = proto;
        this.mapper = mapper;
        this.locator = locator;
        this.secret = secret == null ? "" : secret;
        this.budgetMillis = budgetMillis;
        Map<String, ManagedChannel> configured = new HashMap<>();
        try {
            JsonNode targets = mapper.reader().with(JsonParser.Feature.STRICT_DUPLICATE_DETECTION)
                    .with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS)
                    .readTree(targetsJson == null || targetsJson.isBlank() ? "{}" : targetsJson);
            if (!targets.isObject() || targets.size() > 64) throw new IllegalArgumentException("场景地址映射非法");
            var entries = targets.fields();
            while (entries.hasNext()) {
                var entry = entries.next();
                nodeId(entry.getKey());
                if (!entry.getValue().isTextual() || entry.getValue().textValue().isBlank()) {
                    throw new IllegalArgumentException("场景地址映射必须是节点 ID 到部署地址");
                }
                configured.put(entry.getKey(), channels.apply(entry.getValue().textValue()));
            }
        } catch (IOException error) { throw new IllegalArgumentException("场景地址映射无法解析"); }
        scenes = Map.copyOf(configured);
        if (!scenes.isEmpty()) proto.descriptor(EXECUTE);
    }

    boolean enabled() {
        return locator != null && !scenes.isEmpty() && secret.strip().getBytes(StandardCharsets.UTF_8).length >= 32;
    }

    ObjectNode execute(ObjectNode frozen) {
        if (!enabled()) throw failure("NOT_CONFIGURED", "在线玩家管理服务尚未配置", false, 0);
        if (frozen == null || frozen.size() != FIELDS.size()) throw invalid("玩家命令字段不完整");
        frozen.fieldNames().forEachRemaining(name -> { if (!FIELDS.contains(name)) throw invalid("玩家命令包含未允许字段"); });
        String operation = text(frozen, "operation");
        String command = COMMANDS.get(operation);
        if (command == null) throw invalid("玩家操作未开放");
        String requestId = decimal(frozen.get("requestId"), "请求 ID");
        String playerId = decimal(frozen.get("playerId"), "玩家 ID");
        String actor = text(frozen, "operator");
        String reason = text(frozen, "reason");
        if (actor.isBlank() || actor.getBytes(StandardCharsets.UTF_8).length > 64 || actor.contains("|")
                || actor.chars().anyMatch(Character::isISOControl)) throw invalid("操作人非法");
        if (reason.isBlank() || reason.getBytes(StandardCharsets.UTF_8).length > 768
                || reason.chars().anyMatch(Character::isISOControl)) throw invalid("处置理由非法");
        JsonNode issuedNode = frozen.get("issuedAtMs");
        String issuedText = issuedNode != null && issuedNode.isIntegralNumber() && issuedNode.canConvertToLong()
                ? issuedNode.asText() : decimal(issuedNode, "冻结时间");
        long issued;
        try { issued = Long.parseLong(issuedText); }
        catch (NumberFormatException error) { throw invalid("冻结时间非法"); }
        long now = System.currentTimeMillis();
        if (issued <= 0 || issued < now - 300_000 || issued > now + 300_000) throw invalid("玩家命令已过期，请重新预览");
        if (!(frozen.get("arguments") instanceof ObjectNode arguments)) throw invalid("操作参数必须是对象");
        validateArguments(operation, arguments);

        // 业务字段仍用已冻结原件；准入身份由可信定位服务在执行前获取，再一同签名。
        ObjectNode wire = mapper.createObjectNode().put("commandId", requestId).put("playerId", playerId)
                .put("operator", actor).put("reason", reason).put("issuedAtMs", Long.toString(issued));
        wire.set(command, arguments.deepCopy());
        Deadline deadline = Deadline.after(budgetMillis, TimeUnit.MILLISECONDS);
        ObjectNode sessionReply = invoke(locator, GameServices.SESSION,
                mapper.createObjectNode().put("playerId", playerId), deadline, false);
        JsonNode session = sessionReply.path("session");
        if (!sessionReply.path("found").asBoolean(false) || !session.isObject()
                || !"SESSION_STATE_ONLINE".equals(session.path("state").asText())) {
            throw failure("PLAYER_OFFLINE", "玩家当前不在线，不能执行在线命令", false, 0);
        }
        if (!playerId.equals(session.path("playerId").asText())) throw failure("INVALID_RESPONSE", "玩家定位结果不匹配", false, 0);
        String targetNodeId = session.path("sceneNodeId").asText();
        long target = nodeId(targetNodeId);
        ManagedChannel scene = scenes.get(targetNodeId);
        if (scene == null) throw failure("SCENE_NOT_CONFIGURED", "玩家所在场景未接入管理服务", false, 0);
        long sessionId = uint32(session.get("sessionId"), "会话 ID", false);
        String generation = decimal(session.get("loginGeneration"), "登录代次");
        wire.put("targetNodeId", target).put("expectedSessionId", sessionId).put("expectedLoginGeneration", generation);
        DynamicMessage request;
        try {
            request = proto.request(EXECUTE, wire);
            request = proto.signPlayerCommand(EXECUTE, request, secret, issued / 1000, requestId);
        } catch (IOException | IllegalArgumentException error) { throw invalid("玩家命令不符合游戏协议"); }
        ObjectNode response = invoke(scene, EXECUTE, request, deadline, true);
        int tip = response.path("errorMessage").path("id").asInt(0);
        if (tip != 0) throw failure("SERVER_TIP", "服务器未确认命令完成，请保留原工单核对", true, tip);
        if (!requestId.equals(response.path("commandId").asText()) || !response.path("executed").asBoolean(false)) {
            throw failure("INVALID_RESPONSE", "玩家命令缺少匹配的完成凭据", true, 0);
        }
        return response;
    }

    private void validateArguments(String operation, ObjectNode arguments) {
        Set<String> allowed = switch (operation) {
            case "currency.add", "currency.deduct" -> Set.of("currencyType", "amount");
            case "currency.block", "currency.unblock" -> Set.of("currencyType");
            case "player.level" -> Set.of("level");
            case "pet.grant" -> Set.of("petTableId");
            case "item.grant" -> Set.of("configId", "count");
            default -> throw invalid("玩家操作未开放");
        };
        if (arguments.size() != allowed.size()) throw invalid("操作参数字段不完整");
        arguments.fieldNames().forEachRemaining(name -> { if (!allowed.contains(name)) throw invalid("操作包含未允许参数"); });
        for (String field : allowed) {
            if (field.equals("amount")) {
                String amount = decimal(arguments.get(field), "金额");
                if (new BigInteger(amount).compareTo(BigInteger.valueOf(Long.MAX_VALUE)) > 0) throw invalid("金额超出游戏协议范围");
            } else {
                long number = uint32(arguments.get(field), field, field.equals("currencyType"));
                if (field.equals("currencyType") && !proto.supportsCurrency((int) number)) throw invalid("币种不在游戏协议目录中");
                if (field.equals("level") && number > MAX_PLAYER_LEVEL) throw invalid("玩家等级必须在 1 至 85 之间");
                if ((field.equals("configId") || field.equals("petTableId"))
                        && number > Integer.MAX_VALUE) throw invalid(field + "超出已开放的游戏管理范围");
                // 对齐既有 PlayerEquipSystem::kGmGrantMaxCount；邮件附件使用另一条正式发放规则。
                if (field.equals("count") && number > 99) throw invalid("单次在线 GM 发物不能超过 99 个");
            }
        }
    }

    private ObjectNode invoke(ManagedChannel channel, String method, ObjectNode input, Deadline deadline, boolean writing) {
        try { return invoke(channel, method, proto.request(method, input), deadline, writing); }
        catch (IOException error) { throw invalid("游戏请求无法解析"); }
    }

    private ObjectNode invoke(ManagedChannel channel, String method, DynamicMessage input, Deadline deadline, boolean writing) {
        try {
            DynamicMessage response = ClientCalls.blockingUnaryCall(channel, proto.method(method),
                    CallOptions.DEFAULT.withDeadline(deadline), input);
            return proto.json(response, mapper);
        } catch (StatusRuntimeException error) {
            Status.Code code = error.getStatus().getCode();
            boolean refused = code == Status.Code.UNAUTHENTICATED || code == Status.Code.PERMISSION_DENIED
                    || code == Status.Code.INVALID_ARGUMENT || code == Status.Code.UNIMPLEMENTED;
            throw failure(code.name(), writing && !refused ? "玩家命令结果未知，请保留原工单核对"
                    : "游戏服务调用未完成：" + code.name(), writing && !refused, 0);
        } catch (IOException error) { throw failure("INVALID_RESPONSE", "游戏服务响应无法解析", writing, 0); }
    }

    private static String text(ObjectNode input, String key) {
        JsonNode value = input.get(key);
        if (value == null || !value.isTextual()) throw invalid(key + "必须是文本");
        return value.textValue();
    }

    private static String decimal(JsonNode value, String name) {
        if (value == null || !value.isTextual() || !value.textValue().matches("[1-9][0-9]{0,19}")
                || new BigInteger(value.textValue()).compareTo(UINT64_MAX) > 0) throw invalid(name + "必须是非零 uint64 十进制字符串");
        return value.textValue();
    }

    private static long nodeId(String value) {
        if (value == null || !value.matches("[1-9][0-9]{0,9}")) throw invalid("场景节点 ID 非法");
        long id = Long.parseLong(value);
        if (id > 0xffff_ffffL) throw invalid("场景节点 ID 非法");
        return id;
    }

    private static long uint32(JsonNode value, String name, boolean zero) {
        if (value == null || !value.isIntegralNumber() || !value.canConvertToLong()
                || value.longValue() < (zero ? 0 : 1) || value.longValue() > 0xffff_ffffL) throw invalid(name + "必须是游戏协议范围内的整数");
        return value.longValue();
    }

    private static GameServices.CallFailure invalid(String message) { return failure("INVALID_ARGUMENT", message, false, 0); }
    private static GameServices.CallFailure failure(String code, String message, boolean unknown, int tip) {
        return new GameServices.CallFailure(code, message, unknown, tip);
    }
}
