package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
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
    static final String SEND_SYSTEM_MAIL = "mailpb.MailAdmin/SendSystemMail";
    static final String IMPORT_RECIPIENTS = "mailpb.MailAdmin/ImportMailRecipients";
    static final String SEAL_CAMPAIGN = "mailpb.MailAdmin/SealMailCampaign";
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
            @Value("${gm.game.scene-etcd-url:}") String sceneEtcdUrl,
            @Value("${gm.game.scene-admin-secret:}") String sceneSecret) {
        this(GameProto.load(), mapper, dataTarget, locatorTarget, mailTarget, mailSecret, plaintext, 5000,
                sceneTargetsJson, sceneEtcdUrl, sceneSecret, GamePlayerAdmin.DEFAULT_DISCOVERY_TTL_MS);
    }

    GameServices(GameProto proto, ObjectMapper mapper, String dataTarget, String locatorTarget,
            String mailTarget, String mailSecret, boolean plaintext, long budgetMillis) {
        this(proto, mapper, dataTarget, locatorTarget, mailTarget, mailSecret, plaintext, budgetMillis,
                "{}", "", "", GamePlayerAdmin.DEFAULT_DISCOVERY_TTL_MS);
    }

    GameServices(GameProto proto, ObjectMapper mapper, String dataTarget, String locatorTarget,
            String mailTarget, String mailSecret, boolean plaintext, long budgetMillis, String sceneTargetsJson, String sceneSecret) {
        this(proto, mapper, dataTarget, locatorTarget, mailTarget, mailSecret, plaintext, budgetMillis,
                sceneTargetsJson, "", sceneSecret, GamePlayerAdmin.DEFAULT_DISCOVERY_TTL_MS);
    }

    GameServices(GameProto proto, ObjectMapper mapper, String dataTarget, String locatorTarget,
            String mailTarget, String mailSecret, boolean plaintext, long budgetMillis, String sceneTargetsJson,
            String sceneEtcdUrl, String sceneSecret, long discoveryTtlMs) {
        this.proto = proto;
        this.mapper = mapper;
        this.mailSecret = mailSecret == null ? "" : mailSecret.strip();
        if (budgetMillis <= 0 || budgetMillis > 5000) throw new IllegalArgumentException("游戏调用预算非法");
        this.budgetMillis = budgetMillis;
        GamePlayerAdmin admin = null;
        try {
            data = managedChannel(dataTarget, plaintext);
            locator = managedChannel(locatorTarget, plaintext);
            mail = managedChannel(mailTarget, plaintext);
            admin = new GamePlayerAdmin(proto, mapper, locator, sceneTargetsJson, sceneEtcdUrl, sceneSecret,
                    budgetMillis, discoveryTtlMs, target -> createChannel(target, plaintext));
            playerAdmin = admin;
        } catch (RuntimeException error) {
            if (admin != null) admin.close();
            closeFixedChannels();
            throw error;
        }
    }

    public ArrayNode currencyTypes() { return proto.currencyTypes(); }

    public boolean supportsCurrency(int currencyType) { return proto.supportsCurrency(currencyType); }

    public boolean mailEnabled() {
        return mail != null && mailSecret.getBytes(StandardCharsets.UTF_8).length >= 32;
    }

    public boolean playerCommandsEnabled() { return playerAdmin.enabled(); }

    public ObjectNode sendPlayerCommand(ObjectNode frozenRequest) { return playerAdmin.execute(frozenRequest); }

    private ManagedChannel managedChannel(String target, boolean plaintext) {
        ManagedChannel channel = createChannel(target, plaintext);
        if (channel != null) channels.add(channel);
        return channel;
    }

    private ManagedChannel createChannel(String target, boolean plaintext) {
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
        return builder.build();
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
        ObjectNode sessionReply = readJson(locator, SESSION,
                mapper.createObjectNode().put("playerId", playerId).put("includePlacement", true), deadline);
        boolean found = sessionReply.path("found").asBoolean(false);
        result.put("sessionFound", found);
        if (found) {
            JsonNode session = sessionReply.path("session");
            if (!session.isObject() || !playerId.equals(session.path("playerId").asText())) {
                throw new CallFailure("INVALID_RESPONSE", "会话响应与目标玩家不一致", false, 0);
            }
            JsonNode placement = sessionReply.path("placement");
            boolean hasPlacement = placement.isObject() && placement.path("found").asBoolean(false);
            ObjectNode view = result.putObject("session");
            view.put("state", session.path("state").asText("SESSION_STATE_UNKNOWN"));
            view.put("sceneNodeId", hasPlacement ? placement.path("nodeId").asText("") : session.path("sceneNodeId").asText(""));
            view.put("sceneId", hasPlacement ? placement.path("sceneId").asText("0") : session.path("sceneId").asText("0"));
            view.put("lastActiveTs", session.path("lastActiveTs").asText("0"));
            if (hasPlacement) {
                view.put("sceneZoneId", placement.path("zoneId").asLong(0));
                if (placement.path("awaitingPlacement").asBoolean(false)) view.put("awaitingPlacement", true);
            }
        }
        return result;
    }

    /** 输入必须是已持久冻结的 SendMailRequest；成功仅表示邮件投递成功。 */
    public ObjectNode sendMail(ObjectNode frozenRequest) {
        checkMailConfigured();
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
        return verifiedMailReply(reply);
    }

    /** 输入必须是已持久冻结的纯文本全服系统邮件 SendMailRequest；不允许携带附件或活动名单。 */
    public ObjectNode sendSystemMail(ObjectNode frozenRequest) {
        checkMailConfigured();
        if (frozenRequest == null || frozenRequest.has("auth")) {
            throw invalid("系统邮件请求非法，调用证明由服务端生成");
        }
        ObjectNode input = frozenRequest.deepCopy();
        requireStringId(input, "requestId", "request_id");
        requireStringId(input, "expireMs", "expire_ms");
        DynamicMessage request;
        try { request = proto.request(SEND_SYSTEM_MAIL, input); }
        catch (IOException | IllegalArgumentException error) { throw invalid("系统邮件请求不符合游戏协议"); }
        Descriptors.Descriptor type = request.getDescriptorForType();
        if (!Integer.valueOf(1).equals(request.getField(type.findFieldByName("sender_kind")))) {
            throw invalid("系统邮件发件角色必须是管理工具");
        }
        for (String field : List.of("recipient_player_id", "guild_id", "campaign_id", "manifest_generation",
                "expected_recipient_count", "expected_shard_count")) {
            if (((Number) request.getField(type.findFieldByName(field))).longValue() != 0) {
                throw invalid("纯文本全服系统邮件不得携带单人收件或活动名单字段");
            }
        }
        if (!request.getField(type.findFieldByName("manifest_hash")).equals("")) {
            throw invalid("纯文本全服系统邮件不得携带名单摘要");
        }
        if (hasAttachments(request)) {
            throw invalid("携带附件的系统邮件必须走冻结名单活动流程");
        }
        request = proto.sign(SEND_SYSTEM_MAIL, request, "admin", mailSecret, System.currentTimeMillis());
        DynamicMessage reply = call(mail, SEND_SYSTEM_MAIL, request, Deadline.after(budgetMillis, TimeUnit.MILLISECONDS), true);
        return verifiedMailReply(reply);
    }

    /**
     * 三段式幂等批量活动发奖：SendSystemMail（建活动）→ ImportMailRecipients（逐片导入）→ SealMailCampaign（封存发布）。
     * 任一阶段失败在建活动后均标记 outcomeUnknown=true，支持沿用同一冻结命令安全重试，绝不重复发布或部分发布。
     */
    public ObjectNode sendMailCampaign(ObjectNode frozenCampaign) {
        checkMailConfigured();
        if (frozenCampaign == null || !(frozenCampaign.get("mail") instanceof ObjectNode mailNode)
                || !(frozenCampaign.get("shards") instanceof ArrayNode shardsNode) || shardsNode.isEmpty()) {
            throw invalid("活动邮件草稿缺少冻结请求或名单分片");
        }
        if (mailNode.has("auth")) throw invalid("活动邮件请求非法，调用证明由服务端生成");
        ObjectNode input = mailNode.deepCopy();
        requireStringId(input, "requestId", "request_id");
        requireStringId(input, "expireMs", "expire_ms");
        requireStringId(input, "campaignId", "campaign_id");
        requireStringId(input, "manifestGeneration", "manifest_generation");
        requireStringId(input, "expectedRecipientCount", "expected_recipient_count");

        List<List<String>> shards = new ArrayList<>(shardsNode.size());
        long totalRecipients = 0;
        for (JsonNode shardNode : shardsNode) {
            if (!shardNode.isArray() || shardNode.isEmpty() || shardNode.size() > MailCampaignManifest.SHARD_SIZE) {
                throw invalid("活动名单分片格式非法");
            }
            List<String> ids = new ArrayList<>(shardNode.size());
            for (JsonNode idNode : shardNode) {
                if (!idNode.isTextual()) throw invalid("活动收件人 ID 必须是字符串");
                positiveUint64(idNode.textValue(), "收件人 ID");
                ids.add(idNode.textValue());
            }
            shards.add(ids);
            totalRecipients += ids.size();
        }
        DynamicMessage createReq;
        try { createReq = proto.request(SEND_SYSTEM_MAIL, input); }
        catch (IOException | IllegalArgumentException error) { throw invalid("活动邮件请求不符合游戏协议"); }
        Descriptors.Descriptor type = createReq.getDescriptorForType();
        if (!Integer.valueOf(1).equals(createReq.getField(type.findFieldByName("sender_kind")))) {
            throw invalid("活动邮件发件角色必须是管理工具");
        }
        if (((Number) createReq.getField(type.findFieldByName("recipient_player_id"))).longValue() != 0
                || ((Number) createReq.getField(type.findFieldByName("guild_id"))).longValue() != 0) {
            throw invalid("系统活动邮件不得指定单人或帮会目标");
        }
        if (!hasAttachments(createReq)) {
            throw invalid("系统活动邮件必须包含至少一项货币或物品附件");
        }
        long expectedCount = ((Number) createReq.getField(type.findFieldByName("expected_recipient_count"))).longValue();
        int expectedShards = ((Number) createReq.getField(type.findFieldByName("expected_shard_count"))).intValue();
        String expectedHash = (String) createReq.getField(type.findFieldByName("manifest_hash"));
        String computedHash;
        try { computedHash = MailCampaignManifest.manifestHashOf(shards); }
        catch (IllegalArgumentException error) { throw invalid(error.getMessage()); }
        if (expectedShards != shards.size() || expectedCount != totalRecipients || !computedHash.equals(expectedHash)) {
            throw invalid("活动名单分片与冻结摘要不一致");
        }

        String campaignId = Long.toUnsignedString(((Number) createReq.getField(type.findFieldByName("campaign_id"))).longValue());
        String generation = Long.toUnsignedString(((Number) createReq.getField(type.findFieldByName("manifest_generation"))).longValue());

        // 阶段 1：创建或幂等重放活动意图（此时尚未发布可见）。
        DynamicMessage signedCreate = proto.sign(SEND_SYSTEM_MAIL, createReq, "admin", mailSecret, System.currentTimeMillis());
        DynamicMessage createReply = call(mail, SEND_SYSTEM_MAIL, signedCreate, Deadline.after(budgetMillis, TimeUnit.MILLISECONDS), true);
        verifiedMailReply(createReply);

        // 阶段 2 & 3：活动意图已在邮件服落盘，后续任一故障一律按 outcomeUnknown=true 保留原单待安全重试。
        long importedTotal = 0;
        try {
            for (int index = 0; index < shards.size(); index++) {
                List<String> shard = shards.get(index);
                ObjectNode importJson = mapper.createObjectNode()
                        .put("senderKind", 1)
                        .put("campaignId", campaignId)
                        .put("manifestGeneration", generation)
                        .put("shardIndex", index);
                ArrayNode playerArray = importJson.putArray("playerIds");
                shard.forEach(playerArray::add);
                DynamicMessage importReq = proto.request(IMPORT_RECIPIENTS, importJson);
                importReq = proto.sign(IMPORT_RECIPIENTS, importReq, "admin", mailSecret, System.currentTimeMillis());
                DynamicMessage importReply = call(mail, IMPORT_RECIPIENTS, importReq, Deadline.after(budgetMillis, TimeUnit.MILLISECONDS), true);
                int tip = tipId(importReply);
                if (tip != 0) throw new CallFailure("SERVER_TIP", "导入活动名单分片未确认，请保留原单重试", true, tip);
                long imported = ((Number) importReply.getField(importReply.getDescriptorForType().findFieldByName("imported_count"))).longValue();
                if (imported != shard.size()) {
                    throw new CallFailure("INVALID_RESPONSE", "活动名单分片导入数量不匹配", true, 0);
                }
                importedTotal += imported;
            }
            ObjectNode sealJson = mapper.createObjectNode()
                    .put("senderKind", 1)
                    .put("campaignId", campaignId)
                    .put("manifestGeneration", generation);
            DynamicMessage sealReq = proto.request(SEAL_CAMPAIGN, sealJson);
            sealReq = proto.sign(SEAL_CAMPAIGN, sealReq, "admin", mailSecret, System.currentTimeMillis());
            DynamicMessage sealReply = call(mail, SEAL_CAMPAIGN, sealReq, Deadline.after(budgetMillis, TimeUnit.MILLISECONDS), true);
            ObjectNode result = verifiedMailReply(sealReply);
            result.put("campaignId", campaignId)
                    .put("manifestGeneration", generation)
                    .put("importedRecipients", importedTotal)
                    .put("importedShards", shards.size())
                    .put("sealed", true);
            return result;
        } catch (CallFailure error) {
            if (error.isOutcomeUnknown()) throw error;
            throw new CallFailure(error.getCode(), "活动邮件发布中途未完成，请沿用原命令重试：" + error.getCode(), true, error.getTipId());
        } catch (IOException error) {
            throw new CallFailure("INVALID_RESPONSE", "活动邮件协议序列化失败", true, 0);
        }
    }

    private void checkMailConfigured() {
        require(mail, "邮件服务");
        if (mailSecret.getBytes(StandardCharsets.UTF_8).length < 32) {
            throw new CallFailure("NOT_CONFIGURED", "邮件管理签名密钥未配置", false, 0);
        }
    }

    private ObjectNode verifiedMailReply(DynamicMessage reply) {
        int tip = tipId(reply);
        if (tip != 0) {
            // 不在这里复制导表器的故障码表。所有非零 tip 都保留原号并保守等待原请求核对。
            throw new CallFailure("SERVER_TIP", "服务器未确认邮件投递，请保留原请求核对", true, tip);
        }
        long mailId = ((Number) reply.getField(reply.getDescriptorForType().findFieldByName("mail_id"))).longValue();
        if (mailId == 0) throw new CallFailure("INVALID_RESPONSE", "邮件响应缺少投递凭据", true, 0);
        try { return proto.json(reply, mapper); }
        catch (IOException errorJson) { throw new CallFailure("INVALID_RESPONSE", "邮件响应无法解析", true, 0); }
    }

    private static int tipId(DynamicMessage reply) {
        Descriptors.FieldDescriptor errorField = reply.getDescriptorForType().findFieldByName("error_message");
        DynamicMessage error = (DynamicMessage) reply.getField(errorField);
        return ((Number) error.getField(error.getDescriptorForType().findFieldByName("id"))).intValue();
    }

    private static boolean hasAttachments(DynamicMessage request) {
        Descriptors.FieldDescriptor field = request.getDescriptorForType().findFieldByName("attachments");
        if (!request.hasField(field)) return false;
        DynamicMessage attachments = (DynamicMessage) request.getField(field);
        Descriptors.Descriptor attType = attachments.getDescriptorForType();
        return attachments.getRepeatedFieldCount(attType.findFieldByName("currencies")) > 0
                || attachments.getRepeatedFieldCount(attType.findFieldByName("items")) > 0;
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

    private void closeFixedChannels() {
        for (ManagedChannel channel : channels) channel.shutdownNow();
        for (ManagedChannel channel : channels) {
            try { channel.awaitTermination(2, TimeUnit.SECONDS); }
            catch (InterruptedException error) { Thread.currentThread().interrupt(); return; }
        }
    }

    @PreDestroy
    public void close() {
        if (playerAdmin != null) playerAdmin.close();
        closeFixedChannels();
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
