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
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.HashSet;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;

/** 七项明确的在线玩家管理操作；节点地址仅来自静态部署映射或可信 etcd 注册表，不接受浏览器传入地址。 */
final class GamePlayerAdmin {
    static final String EXECUTE = "scene_node.SceneNodeGrpc/GmExecutePlayerCommand";
    // 对齐 cpp/libs/services/scene/player/system/player_level_rules.h 的可写等级范围。
    static final int MAX_PLAYER_LEVEL = 85;
    static final long DEFAULT_DISCOVERY_TTL_MS = 5_000L;
    private static final long MISS_REFRESH_COOLDOWN_MS = 1_000L;
    private static final int MAX_SCENE_CHANNELS = 1024;
    private static final int MAX_ETCD_RESPONSE_BYTES = 4 * 1024 * 1024;
    private static final String SCENE_RPC_PREFIX = "SceneNodeService.rpc/";
    private static final String SCENE_RPC_RANGE_END = "SceneNodeService.rpc0";
    private static final String ETCD_RANGE_BODY = "{\"key\":\""
            + Base64.getEncoder().encodeToString(SCENE_RPC_PREFIX.getBytes(StandardCharsets.UTF_8))
            + "\",\"range_end\":\""
            + Base64.getEncoder().encodeToString(SCENE_RPC_RANGE_END.getBytes(StandardCharsets.UTF_8))
            + "\"}";
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
    private final Map<String, ManagedChannel> staticScenes;
    private final URI sceneEtcdRangeUri;
    private final Function<String, ManagedChannel> channelFactory;
    private final String secret;
    private final long budgetMillis;
    private final long discoveryTtlMs;
    private final HttpClient http;
    private final Object discoveryLock = new Object();
    private final LinkedHashMap<String, SceneChannel> dynamicScenes = new LinkedHashMap<>(16, 0.75f, true);
    private Map<String, String> discoveredByZoneNode = Map.of();
    private Set<String> ambiguousZoneNodes = Set.of();
    private Map<String, String> discoveredByNodeOnly = Map.of();
    private Set<String> ambiguousNodeOnly = Set.of();
    private long lastDiscoveryMs;

    GamePlayerAdmin(GameProto proto, ObjectMapper mapper, ManagedChannel locator, String targetsJson,
            String secret, long budgetMillis, Function<String, ManagedChannel> channels) {
        this(proto, mapper, locator, targetsJson, "", secret, budgetMillis, DEFAULT_DISCOVERY_TTL_MS, channels);
    }

    GamePlayerAdmin(GameProto proto, ObjectMapper mapper, ManagedChannel locator, String targetsJson,
            String sceneEtcdUrl, String secret, long budgetMillis, long discoveryTtlMs,
            Function<String, ManagedChannel> channels) {
        this.proto = proto;
        this.mapper = mapper;
        this.locator = locator;
        this.secret = secret == null ? "" : secret.strip();
        this.budgetMillis = budgetMillis;
        this.discoveryTtlMs = Math.max(0L, discoveryTtlMs);
        this.channelFactory = channels;
        this.sceneEtcdRangeUri = parseEtcdRangeUri(sceneEtcdUrl);
        this.http = this.sceneEtcdRangeUri == null ? null
                : HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2))
                        .followRedirects(HttpClient.Redirect.NEVER).build();
        Map<String, ManagedChannel> configured = new HashMap<>();
        try {
            JsonNode targets = mapper.reader().with(JsonParser.Feature.STRICT_DUPLICATE_DETECTION)
                    .with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS)
                    .readTree(targetsJson == null || targetsJson.isBlank() ? "{}" : targetsJson);
            if (!targets.isObject() || targets.size() > MAX_SCENE_CHANNELS) {
                throw new IllegalArgumentException("场景地址映射非法");
            }
            var entries = targets.fields();
            while (entries.hasNext()) {
                var entry = entries.next();
                validateStaticTargetKey(entry.getKey());
                if (!entry.getValue().isTextual() || entry.getValue().textValue().isBlank()) {
                    throw new IllegalArgumentException("场景地址映射必须是节点 ID 或区/节点 ID 到部署地址");
                }
                configured.put(entry.getKey(), channels.apply(entry.getValue().textValue()));
            }
        } catch (IOException error) {
            shutdownChannels(configured.values());
            throw new IllegalArgumentException("场景地址映射无法解析");
        } catch (RuntimeException error) {
            shutdownChannels(configured.values());
            throw error;
        }
        staticScenes = Map.copyOf(configured);
        if (!staticScenes.isEmpty() || sceneEtcdRangeUri != null) proto.descriptor(EXECUTE);
    }

    boolean enabled() {
        return locator != null && (!staticScenes.isEmpty() || sceneEtcdRangeUri != null)
                && secret.getBytes(StandardCharsets.UTF_8).length >= 32;
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

        // 业务字段仍用已冻结原件；准入身份与权威场景落点在执行前向 PlayerLocator 同槽原子获取，再一同签名。
        ObjectNode wire = mapper.createObjectNode().put("commandId", requestId).put("playerId", playerId)
                .put("operator", actor).put("reason", reason).put("issuedAtMs", Long.toString(issued));
        wire.set(command, arguments.deepCopy());
        Deadline deadline = Deadline.after(budgetMillis, TimeUnit.MILLISECONDS);
        ObjectNode sessionReply = invoke(locator, GameServices.SESSION,
                mapper.createObjectNode().put("playerId", playerId).put("includePlacement", true), deadline, false);
        JsonNode session = sessionReply.path("session");
        if (!sessionReply.path("found").asBoolean(false) || !session.isObject()
                || !"SESSION_STATE_ONLINE".equals(session.path("state").asText())) {
            throw failure("PLAYER_OFFLINE", "玩家当前不在线，不能执行在线命令", false, 0);
        }
        if (!playerId.equals(session.path("playerId").asText())) throw failure("INVALID_RESPONSE", "玩家定位结果不匹配", false, 0);
        long sessionId = uint32(session.get("sessionId"), "会话 ID", false);
        String generation = decimal(session.get("loginGeneration"), "登录代次");

        String targetNodeId;
        long targetZoneId;
        if (sessionReply.has("placement") && sessionReply.path("placement").isObject()) {
            JsonNode placement = sessionReply.path("placement");
            if (!placement.path("found").asBoolean(false)) {
                throw failure("PLAYER_NOT_IN_SCENE", "玩家尚未进入场景，不能执行在线命令", false, 0);
            }
            if (placement.path("awaitingPlacement").asBoolean(false)) {
                throw failure("PLAYER_HANDOFF", "玩家正在跨区或切图交接中，请稍后重试", false, 0);
            }
            long placementSessionId = placement.path("sessionId").asLong(0);
            String placementGeneration = placement.path("loginGeneration").asText("0");
            if ((placementSessionId != 0 && placementSessionId != sessionId)
                    || (!placementGeneration.isEmpty() && !placementGeneration.equals("0") && !placementGeneration.equals(generation))) {
                throw failure("SESSION_MISMATCH", "玩家会话与场景落点尚未同步，请稍后重试", false, 0);
            }
            targetNodeId = placement.path("nodeId").asText("");
            targetZoneId = placement.path("zoneId").asLong(0);
        } else {
            // 兼容尚未升级的旧版 player_locator 应答；新版始终返回 placement。
            targetNodeId = session.path("sceneNodeId").asText("");
            targetZoneId = session.path("gateZoneId").asLong(0);
        }
        long target = nodeId(targetNodeId);
        ManagedChannel scene = resolveScene(targetZoneId, targetNodeId, deadline);
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

    void close() {
        List<ManagedChannel> all = new ArrayList<>(staticScenes.values());
        synchronized (discoveryLock) {
            for (SceneChannel entry : dynamicScenes.values()) all.add(entry.channel);
            dynamicScenes.clear();
        }
        shutdownChannels(all);
    }

    private ManagedChannel resolveScene(long zoneId, String targetNodeId, Deadline deadline) {
        if (zoneId > 0) {
            ManagedChannel byZone = staticScenes.get(zoneId + "/" + targetNodeId);
            if (byZone != null) return byZone;
        }
        ManagedChannel configured = staticScenes.get(targetNodeId);
        if (configured != null) return configured;
        if (sceneEtcdRangeUri == null) throw failure("SCENE_NOT_CONFIGURED", "玩家所在场景未接入管理服务", false, 0);
        String lookupKey = zoneId > 0 ? (zoneId + "/" + targetNodeId) : targetNodeId;
        synchronized (discoveryLock) {
            long nowMs = System.currentTimeMillis();
            boolean expired = lastDiscoveryMs == 0 || (nowMs - lastDiscoveryMs >= discoveryTtlMs);
            boolean known = zoneId > 0
                    ? (discoveredByZoneNode.containsKey(lookupKey) || ambiguousZoneNodes.contains(lookupKey))
                    : (discoveredByNodeOnly.containsKey(lookupKey) || ambiguousNodeOnly.contains(lookupKey));
            boolean missRefreshable = !known && (nowMs - lastDiscoveryMs >= Math.min(MISS_REFRESH_COOLDOWN_MS, discoveryTtlMs));
            if (expired || missRefreshable) refreshFromEtcd(deadline);
            boolean ambiguous = zoneId > 0 ? ambiguousZoneNodes.contains(lookupKey) : ambiguousNodeOnly.contains(lookupKey);
            if (ambiguous) {
                throw failure("SCENE_AMBIGUOUS", "目标场景节点存在重复注册，已拒绝执行", false, 0);
            }
            String endpoint = zoneId > 0 ? discoveredByZoneNode.get(lookupKey) : discoveredByNodeOnly.get(lookupKey);
            if (endpoint == null) throw failure("SCENE_NOT_CONFIGURED", "玩家所在场景未接入管理服务", false, 0);
            SceneChannel existing = dynamicScenes.get(lookupKey);
            if (existing != null && existing.endpoint.equals(endpoint)) return existing.channel;
            if (existing != null) {
                dynamicScenes.remove(lookupKey);
                existing.channel.shutdown();
            }
            if (dynamicScenes.size() >= MAX_SCENE_CHANNELS) {
                Iterator<Map.Entry<String, SceneChannel>> it = dynamicScenes.entrySet().iterator();
                if (it.hasNext()) {
                    SceneChannel evicted = it.next().getValue();
                    it.remove();
                    evicted.channel.shutdown();
                }
            }
            ManagedChannel created;
            try { created = channelFactory.apply(endpoint); }
            catch (IllegalArgumentException error) {
                throw failure("SCENE_TARGET_INVALID", "发现的场景地址不符合连接安全策略", false, 0);
            }
            dynamicScenes.put(lookupKey, new SceneChannel(endpoint, created));
            return created;
        }
    }

    private void refreshFromEtcd(Deadline deadline) {
        long remainingMs = deadline.timeRemaining(TimeUnit.MILLISECONDS);
        if (remainingMs <= 0) throw failure("DEADLINE_EXCEEDED", "场景发现前调用预算已耗尽", false, 0);
        long timeoutMs = Math.max(200L, Math.min(3_000L, remainingMs));
        HttpRequest request = HttpRequest.newBuilder(sceneEtcdRangeUri)
                .timeout(Duration.ofMillis(timeoutMs))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(ETCD_RANGE_BODY, StandardCharsets.UTF_8))
                .build();
        JsonNode root;
        try {
            HttpResponse<java.io.InputStream> response = http.send(request, HttpResponse.BodyHandlers.ofInputStream());
            try (var stream = response.body()) {
                byte[] bytes = stream.readNBytes(MAX_ETCD_RESPONSE_BYTES + 1);
                if (response.statusCode() != 200 || bytes.length > MAX_ETCD_RESPONSE_BYTES) {
                    throw failure("SCENE_DISCOVERY_UNAVAILABLE", "场景服务发现返回异常状态", false, 0);
                }
                root = mapper.readTree(bytes);
            }
        } catch (GameServices.CallFailure error) {
            throw error;
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
            throw failure("SCENE_DISCOVERY_UNAVAILABLE", "场景服务发现被中断", false, 0);
        } catch (IOException error) {
            throw failure("SCENE_DISCOVERY_UNAVAILABLE", "场景服务发现不可用", false, 0);
        }

        Map<String, String> byZoneNode = new HashMap<>();
        Set<String> dupZoneNode = new HashSet<>();
        Map<String, String> byNodeOnly = new HashMap<>();
        Set<String> dupNodeOnly = new HashSet<>();
        JsonNode kvs = root.path("kvs");
        if (kvs.isArray()) {
            for (JsonNode kv : kvs) {
                String keyText = decodeBase64(kv.path("key").asText(""));
                // noderegistry 在同一前缀下写入 /allocated/ 占位键，不是节点注册记录。
                if (keyText == null || keyText.contains("/allocated/")) continue;
                String valueText = decodeBase64(kv.path("value").asText(""));
                if (valueText == null) continue;
                JsonNode reg;
                try { reg = mapper.readTree(valueText); }
                catch (IOException ignored) { continue; }
                if (!reg.isObject()) continue;
                JsonNode nodeIdField = reg.get("nodeId");
                if (nodeIdField == null || !nodeIdField.isIntegralNumber() || !nodeIdField.canConvertToLong()) continue;
                long id = nodeIdField.longValue();
                if (id <= 0 || id > 0xffff_ffffL) continue;
                long regZone = 0;
                JsonNode zoneIdField = reg.get("zoneId");
                if (zoneIdField != null && zoneIdField.isIntegralNumber() && zoneIdField.canConvertToLong()) {
                    long z = zoneIdField.longValue();
                    if (z > 0 && z <= 0xffff_ffffL) regZone = z;
                }
                // 只认 grpcEndpoint，绝不回退到 endpoint（那是 muduo raw TCP 端口）。
                JsonNode grpcEndpoint = reg.path("grpcEndpoint");
                String ip = grpcEndpoint.path("ip").asText("").strip();
                int port = grpcEndpoint.path("port").asInt(0);
                if (ip.isEmpty() || port < 1 || port > 65535) continue;
                String nodeKey = Long.toString(id);
                String endpoint = formatEndpoint(ip, port);
                recordDiscovery(byNodeOnly, dupNodeOnly, nodeKey, endpoint);
                if (regZone > 0) {
                    recordDiscovery(byZoneNode, dupZoneNode, regZone + "/" + nodeKey, endpoint);
                }
            }
        }
        discoveredByZoneNode = Map.copyOf(byZoneNode);
        ambiguousZoneNodes = Set.copyOf(dupZoneNode);
        discoveredByNodeOnly = Map.copyOf(byNodeOnly);
        ambiguousNodeOnly = Set.copyOf(dupNodeOnly);
        lastDiscoveryMs = System.currentTimeMillis();
        // 清理已下线或更换地址的旧连接，使用优雅关闭以免中断在途调用。
        Iterator<Map.Entry<String, SceneChannel>> it = dynamicScenes.entrySet().iterator();
        while (it.hasNext()) {
            Map.Entry<String, SceneChannel> entry = it.next();
            String key = entry.getKey();
            String latest = key.contains("/") ? discoveredByZoneNode.get(key) : discoveredByNodeOnly.get(key);
            if (latest == null || !latest.equals(entry.getValue().endpoint)) {
                it.remove();
                entry.getValue().channel.shutdown();
            }
        }
    }

    private static void recordDiscovery(Map<String, String> map, Set<String> ambiguous, String key, String endpoint) {
        if (ambiguous.contains(key)) return;
        String prior = map.get(key);
        if (prior != null && !prior.equals(endpoint)) {
            map.remove(key);
            ambiguous.add(key);
        } else {
            map.put(key, endpoint);
        }
    }

    private static void validateStaticTargetKey(String key) {
        if (key == null) throw new IllegalArgumentException("场景节点 ID 非法");
        int slash = key.indexOf('/');
        if (slash < 0) {
            nodeId(key);
            return;
        }
        if (key.indexOf('/', slash + 1) >= 0) throw new IllegalArgumentException("场景节点映射键非法");
        nodeId(key.substring(0, slash));
        nodeId(key.substring(slash + 1));
    }

    private static URI parseEtcdRangeUri(String raw) {
        if (raw == null || raw.isBlank()) return null;
        URI uri;
        try { uri = URI.create(raw.strip()); }
        catch (IllegalArgumentException error) { throw new IllegalArgumentException("场景发现 etcd 地址非法"); }
        String host = uri.getHost();
        boolean loopback = "127.0.0.1".equals(host) || "localhost".equalsIgnoreCase(host) || "[::1]".equals(host) || "::1".equals(host);
        if (host == null || uri.getUserInfo() != null || uri.getQuery() != null || uri.getFragment() != null
                || !("https".equals(uri.getScheme()) || ("http".equals(uri.getScheme()) && loopback))) {
            throw new IllegalArgumentException("场景发现 etcd 仅允许 HTTPS 或本机回环 HTTP");
        }
        String base = uri.toString().replaceAll("/+$", "");
        return URI.create(base + "/v3/kv/range");
    }

    private static String decodeBase64(String encoded) {
        if (encoded == null || encoded.isEmpty()) return null;
        try { return new String(Base64.getDecoder().decode(encoded), StandardCharsets.UTF_8); }
        catch (IllegalArgumentException error) { return null; }
    }

    private static String formatEndpoint(String ip, int port) {
        if (ip.contains(":") && !ip.startsWith("[")) return "[" + ip + "]:" + port;
        return ip + ":" + port;
    }

    private static void shutdownChannels(Iterable<ManagedChannel> channels) {
        for (ManagedChannel channel : channels) channel.shutdownNow();
        for (ManagedChannel channel : channels) {
            try { channel.awaitTermination(2, TimeUnit.SECONDS); }
            catch (InterruptedException error) { Thread.currentThread().interrupt(); return; }
        }
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

    private record SceneChannel(String endpoint, ManagedChannel channel) { }
}
