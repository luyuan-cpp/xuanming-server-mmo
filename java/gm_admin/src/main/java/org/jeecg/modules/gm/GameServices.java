package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import io.grpc.CallOptions;
import io.grpc.Deadline;
import io.grpc.ManagedChannel;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.stub.ClientCalls;
import jakarta.annotation.PreDestroy;
import java.io.IOException;
import java.math.BigInteger;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Lazy;
import org.springframework.stereotype.Service;

/** 独立 GM 的受限游戏服务适配器；不直接访问游戏存储，不模拟玩家会话。 */
@Service
@Lazy(false)
public final class GameServices {
    static final String DISPLAY = "data_service.DataService/BatchGetPlayerDisplay";
    static final String HOME = "data_service.DataService/GetPlayerHomeZone";
    static final String SESSION = "playerlocator.PlayerLocator/GetSession";
    static final String SEND_MAIL = "mailpb.MailAdmin/SendPersonalMail";
    private static final BigInteger MAX_UINT64 = new BigInteger("18446744073709551615");
    private final GameProto proto;
    private final ObjectMapper mapper;
    private final ManagedChannel data;
    private final ManagedChannel locator;
    private final ManagedChannel mail;
    private final String mailSecret;
    private final long budgetMillis;
    private final GamePlayerAdmin playerAdmin;
    private final List<ManagedChannel> channels = new ArrayList<>();

    @Autowired
    public GameServices(ObjectMapper mapper,
            @Value("${gm.game.data-target:}") String dataTarget,
            @Value("${gm.game.locator-target:}") String locatorTarget,
            @Value("${gm.game.mail-target:}") String mailTarget,
            @Value("${gm.game.mail-admin-secret:}") String mailSecret,
            @Value("${gm.game.plaintext:false}") boolean plaintext,
            @Value("${gm.game.scene-targets-json:}") String sceneTargetsJson,
            @Value("${gm.game.scene-admin-secret:}") String sceneSecret) {
        this(GameProto.load(), mapper, dataTarget, locatorTarget, mailTarget, mailSecret, plaintext, 5000,
                sceneTargetsJson, sceneSecret);
    }

    GameServices(GameProto proto, ObjectMapper mapper, String dataTarget, String locatorTarget,
            String mailTarget, String mailSecret, boolean plaintext, long budgetMillis) {
        this(proto, mapper, dataTarget, locatorTarget, mailTarget, mailSecret, plaintext, budgetMillis, "{}", "");
    }

    GameServices(GameProto proto, ObjectMapper mapper, String dataTarget, String locatorTarget,
            String mailTarget, String mailSecret, boolean plaintext, long budgetMillis, String sceneTargetsJson, String sceneSecret) {
        this.proto = proto;
        this.mapper = mapper;
        this.mailSecret = mailSecret == null ? "" : mailSecret.strip();
        if (budgetMillis <= 0 || budgetMillis > 5000) throw new IllegalArgumentException("游戏调用预算非法");
        this.budgetMillis = budgetMillis;
        try {
            data = channel(dataTarget, plaintext);
            locator = channel(locatorTarget, plaintext);
            mail = channel(mailTarget, plaintext);
            playerAdmin = new GamePlayerAdmin(proto, mapper, locator, sceneTargetsJson, sceneSecret, budgetMillis,
                    target -> channel(target, plaintext));
        } catch (RuntimeException error) {
            close();
            throw error;
        }
    }

    public ArrayNode currencyTypes() { return proto.currencyTypes(); }

    public boolean playerCommandsEnabled() { return playerAdmin.enabled(); }

    public ObjectNode sendPlayerCommand(ObjectNode frozenRequest) { return playerAdmin.execute(frozenRequest); }

    private ManagedChannel channel(String target, boolean plaintext) {
        if (target == null || target.isBlank()) return null;
        URI uri;
        try { uri = URI.create("tcp://" + target); }
        catch (IllegalArgumentException error) { throw new IllegalArgumentException("游戏服务地址非法"); }
        String host = uri.getHost();
        if (host == null || uri.getPort() < 1 || uri.getPort() > 65535 || uri.getUserInfo() != null
                || uri.getQuery() != null || uri.getFragment() != null || !uri.getRawPath().isEmpty()) {
            throw new IllegalArgumentException("游戏服务地址必须是 host:port");
        }
        host = host.replace("[", "").replace("]", "");
        boolean loopback = host.equalsIgnoreCase("localhost") || host.equals("127.0.0.1") || host.equals("::1");
        if (plaintext && !loopback) throw new IllegalArgumentException("明文游戏连接只允许显式配置的本机回环地址");
        NettyChannelBuilder builder = NettyChannelBuilder.forAddress(host, uri.getPort())
                .disableRetry().maxInboundMessageSize(1024 * 1024);
        if (plaintext) builder.usePlaintext(); else builder.useTransportSecurity();
        ManagedChannel channel = builder.build();
        channels.add(channel);
        return channel;
    }

    /** 全链路共用一个截止时间；没有查到角色和依赖失败是不同结果。 */
    public ObjectNode player(String playerId) {
        positiveUint64(playerId, "玩家 ID");
        require(data, "玩家资料服务");
        require(locator, "玩家会话服务");
        Deadline deadline = Deadline.after(budgetMillis, TimeUnit.MILLISECONDS);
        ObjectNode displayRequest = mapper.createObjectNode();
        displayRequest.putArray("playerIds").add(playerId);
        ObjectNode displays = readJson(data, DISPLAY, displayRequest, deadline);
        JsonNode display = displays.path("players").path(playerId);
        ObjectNode result = mapper.createObjectNode().put("playerId", playerId).put("found", display.isObject());
        if (!display.isObject()) return result;
        ObjectNode publicDisplay = result.putObject("display");
        publicDisplay.put("name", display.path("name").asText(""));
        publicDisplay.put("level", display.path("level").asLong(0));
        publicDisplay.put("classId", display.path("classId").asLong(0));
        publicDisplay.put("gender", display.path("gender").asLong(0));
        publicDisplay.put("appearanceId", display.path("appearanceId").asText(""));
        ObjectNode byId = mapper.createObjectNode().put("playerId", playerId);
        try {
            ObjectNode home = readJson(data, HOME, byId, deadline);
            result.put("homeZoneFound", true).put("homeZoneId", home.path("homeZoneId").asLong(0));
        } catch (CallFailure error) {
            if (!error.getCode().equals("NOT_FOUND")) throw error;
            result.put("homeZoneFound", false);
        }
        ObjectNode sessionReply = readJson(locator, SESSION, byId, deadline);
        boolean found = sessionReply.path("found").asBoolean(false);
        result.put("sessionFound", found);
        if (found) {
            JsonNode session = sessionReply.path("session");
            if (!session.isObject() || !playerId.equals(session.path("playerId").asText())) {
                throw new CallFailure("INVALID_RESPONSE", "会话响应与目标玩家不一致", false, 0);
            }
            ObjectNode view = result.putObject("session");
            view.put("state", session.path("state").asText("SESSION_STATE_UNKNOWN"));
            view.put("sceneNodeId", session.path("sceneNodeId").asText(""));
            view.put("sceneId", session.path("sceneId").asText("0"));
            view.put("lastActiveTs", session.path("lastActiveTs").asText("0"));
        }
        return result;
    }

    /** 输入必须是已持久冻结的 SendMailRequest；成功仅表示邮件投递成功。 */
    public ObjectNode sendMail(ObjectNode frozenRequest) {
        require(mail, "邮件服务");
        if (mailSecret.getBytes(StandardCharsets.UTF_8).length < 32) {
            throw new CallFailure("NOT_CONFIGURED", "邮件管理签名密钥未配置", false, 0);
        }
        if (frozenRequest == null || frozenRequest.has("auth")) {
            throw invalid("邮件请求非法，调用证明由服务端生成");
        }
        ObjectNode input = frozenRequest.deepCopy();
        requireStringId(input, "requestId", "request_id");
        requireStringId(input, "recipientPlayerId", "recipient_player_id");
        requireStringId(input, "expireMs", "expire_ms");
        DynamicMessage request;
        try { request = proto.request(SEND_MAIL, input); }
        catch (IOException | IllegalArgumentException error) { throw invalid("邮件请求不符合游戏协议"); }
        Descriptors.Descriptor type = request.getDescriptorForType();
        if (!Integer.valueOf(1).equals(request.getField(type.findFieldByName("sender_kind")))) {
            throw invalid("邮件发件角色必须是管理工具");
        }
        for (String field : List.of("guild_id", "campaign_id", "manifest_generation", "expected_recipient_count", "expected_shard_count")) {
            if (((Number) request.getField(type.findFieldByName(field))).longValue() != 0) {
                throw invalid("本入口仅允许给单个玩家发送个人邮件");
            }
        }
        if (!request.getField(type.findFieldByName("manifest_hash")).equals("")) {
            throw invalid("本入口不接受批量发件名单");
        }
        request = proto.sign(SEND_MAIL, request, "admin", mailSecret, System.currentTimeMillis());
        DynamicMessage reply = call(mail, SEND_MAIL, request, Deadline.after(budgetMillis, TimeUnit.MILLISECONDS), true);
        Descriptors.FieldDescriptor errorField = reply.getDescriptorForType().findFieldByName("error_message");
        DynamicMessage error = (DynamicMessage) reply.getField(errorField);
        int tip = ((Number) error.getField(error.getDescriptorForType().findFieldByName("id"))).intValue();
        if (tip != 0) {
            // 不在这里复制导表器的故障码表。所有非零 tip 都保留原号并保守等待原请求核对。
            throw new CallFailure("SERVER_TIP", "服务器未确认邮件投递，请保留原请求核对", true, tip);
        }
        long mailId = ((Number) reply.getField(reply.getDescriptorForType().findFieldByName("mail_id"))).longValue();
        if (mailId == 0) throw new CallFailure("INVALID_RESPONSE", "邮件响应缺少投递凭据", true, 0);
        try { return proto.json(reply, mapper); }
        catch (IOException errorJson) { throw new CallFailure("INVALID_RESPONSE", "邮件响应无法解析", true, 0); }
    }

    private ObjectNode readJson(ManagedChannel channel, String method, ObjectNode request, Deadline deadline) {
        try { return proto.json(call(channel, method, proto.request(method, request), deadline, false), mapper); }
        catch (IOException error) { throw new CallFailure("INVALID_RESPONSE", "游戏查询响应无法解析", false, 0); }
    }

    private DynamicMessage call(ManagedChannel channel, String method, DynamicMessage request,
            Deadline deadline, boolean writing) {
        try {
            return ClientCalls.blockingUnaryCall(channel, proto.method(method),
                    CallOptions.DEFAULT.withDeadline(deadline), request);
        } catch (StatusRuntimeException error) {
            Status.Code code = error.getStatus().getCode();
            boolean refused = code == Status.Code.UNAUTHENTICATED || code == Status.Code.PERMISSION_DENIED
                    || code == Status.Code.INVALID_ARGUMENT || code == Status.Code.UNIMPLEMENTED;
            throw new CallFailure(code.name(), writing && !refused
                    ? "邮件投递结果未知，请保留原请求核对" : "游戏服务调用未完成：" + code.name(), writing && !refused, 0);
        }
    }

    private static void require(ManagedChannel channel, String name) {
        if (channel == null) throw new CallFailure("NOT_CONFIGURED", name + "尚未配置", false, 0);
    }

    private static void requireStringId(ObjectNode input, String camel, String snake) {
        if (input.has(camel) && input.has(snake)) throw invalid("同一字段不能使用两种名称重复提交");
        JsonNode value = input.has(camel) ? input.get(camel) : input.get(snake);
        if (value == null || !value.isTextual()) throw invalid(camel + "必须是十进制字符串");
        positiveUint64(value.textValue(), camel);
    }

    private static void positiveUint64(String value, String field) {
        if (value == null || !value.matches("[1-9][0-9]{0,19}") || new BigInteger(value).compareTo(MAX_UINT64) > 0) {
            throw invalid(field + "必须是非零 uint64 十进制字符串");
        }
    }

    private static CallFailure invalid(String message) {
        return new CallFailure("INVALID_ARGUMENT", message, false, 0);
    }

    @PreDestroy
    public void close() {
        for (ManagedChannel channel : channels) channel.shutdownNow();
        for (ManagedChannel channel : channels) {
            try { channel.awaitTermination(2, TimeUnit.SECONDS); }
            catch (InterruptedException error) { Thread.currentThread().interrupt(); return; }
        }
    }

    public static final class CallFailure extends RuntimeException {
        private final String code;
        private final boolean outcomeUnknown;
        private final int tipId;
        CallFailure(String code, String message, boolean outcomeUnknown, int tipId) {
            super(message);
            this.code = code;
            this.outcomeUnknown = outcomeUnknown;
            this.tipId = tipId;
        }
        public String getCode() { return code; }
        public boolean isOutcomeUnknown() { return outcomeUnknown; }
        public int getTipId() { return tipId; }
    }
}
