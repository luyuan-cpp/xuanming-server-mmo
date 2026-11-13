package com.game.gateway.controller;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.ObjectReader;
import com.fasterxml.jackson.databind.PropertyNamingStrategies;
import com.fasterxml.jackson.databind.annotation.JsonNaming;
import com.game.gateway.service.KillSwitchRuleStore;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.DeleteMapping;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PutMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.server.ResponseStatusException;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.function.Supplier;
import java.util.regex.Pattern;

/**
 * 运维:RPC 热关停规则的唯一管理入口(docs/design/rpc-kill-switch.md §3、§6)。规则写在 etcd {@code /mmorpg/killswitch/},
 * C++ 节点与 Go 服务各自 watch、在本地派发入口判定;本接口只读写规则,不通知任何进程。
 * <ul>
 *   <li>{@code GET /admin/killswitch}:全部规则,含写坏的值({@code valid=false},各进程按不存在处理);</li>
 *   <li>{@code PUT /admin/killswitch {target, node_type?, instance?, deny, reason?, code?}}:写一条规则。
 *       {@code node_type} 与 {@code instance} 都不给 = 全部进程;只给 {@code node_type} = 这一类节点;
 *       给 {@code instance}(必须同时给 {@code node_type},用来在发现里核对这个 uuid 确实在线)= 这一个进程。
 *       类型范围要求发现里至少有一个该类型的节点,防止把类型名写错后规则静默不生效;</li>
 *   <li>{@code DELETE /admin/killswitch?key=<相对键>}:删一条规则,恒 200,{@code deleted} 为删掉的条数。</li>
 * </ul>
 * 鉴权沿用 {@code AdminApiKeyFilter}(X-Admin-Key);写操作打审计日志(键、值、可选的 X-Admin-Operator、来源地址)。
 */
@RestController
@RequestMapping("/admin/killswitch")
public class AdminKillSwitchController {

    /** 与 C++ kill_switch::kDefaultPrefix、Go killswitch.DefaultPrefix 同值。 */
    public static final String PREFIX = "/mmorpg/killswitch/";
    static final String TYPE_SCOPE = "type/";
    static final String INSTANCE_SCOPE = "instance/";
    static final int MAX_TARGET_CHARS = 200;
    static final int MAX_REASON_CHARS = 256;
    static final int MAX_OPERATOR_CHARS = 64;
    /** gRPC 状态码的上界(UNAUTHENTICATED);0 = 缺省 UNAVAILABLE。 */
    static final int MAX_STATUS_CODE = 16;
    static final String OPERATOR_HEADER = "X-Admin-Operator";

    /** 目标只收标识符、点、斜杠、星号(不收空白与控制字符:会进审计日志,也是 etcd 键的一部分)。 */
    private static final Pattern TARGET_CHARS = Pattern.compile("[A-Za-z0-9_.$/*-]+");
    /** eNodeType 枚举名都以 NodeService 结尾(proto/common/base/node.proto)。 */
    private static final Pattern NODE_TYPE = Pattern.compile("[A-Z][A-Za-z0-9]*NodeService");
    /** NodeInfo.node_uuid 的文本形态(C++ boost::uuids / Go google/uuid 都是 8-4-4-4-12 小写十六进制)。 */
    private static final Pattern NODE_UUID =
            Pattern.compile("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}");
    private static final Pattern OPERATOR_CHARS = Pattern.compile("[A-Za-z0-9_.@-]+");

    private static final Logger log = LoggerFactory.getLogger(AdminKillSwitchController.class);
    private static final ObjectMapper JSON = new ObjectMapper();
    /** 同 Go json.Unmarshal:顶层值之后还有内容算写坏。 */
    private static final ObjectReader STRICT_JSON = JSON.reader().with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS);

    private final KillSwitchRuleStore store;

    public AdminKillSwitchController(KillSwitchRuleStore store) {
        this.store = store;
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record RuleRequest(String target, String nodeType, String instance, Boolean deny, String reason, Integer code) {
    }

    /** {@code key} 是去掉前缀的相对键(DELETE 收它);{@code scope} 是 all / type / instance / malformed。 */
    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record RuleView(String key, String scope, String scopeValue, String target, boolean valid, boolean deny,
                           String reason, int code, String raw) {
    }

    @JsonNaming(PropertyNamingStrategies.SnakeCaseStrategy.class)
    public record DeleteResult(String key, long deleted) {
    }

    /** 规则值(与 Go ParseRule、C++ kill_switch::ParseRule 同口径)。 */
    record ParsedRule(boolean deny, String reason, int code) {
    }

    /** 相对键拆出的范围:scope 为 all / type / instance;malformed 时 target 为空。 */
    record ParsedKey(String scope, String scopeValue, String target) {
        boolean malformed() {
            return target.isEmpty();
        }
    }

    @GetMapping
    public List<RuleView> list() {
        Map<String, String> raw = call(() -> store.list(PREFIX));
        List<RuleView> views = new ArrayList<>(raw.size());
        for (Map.Entry<String, String> entry : raw.entrySet()) {
            String relative = entry.getKey().substring(PREFIX.length());
            ParsedKey key = parseKey(relative);
            Optional<ParsedRule> rule = key.malformed() ? Optional.empty() : parseRule(entry.getValue());
            views.add(new RuleView(relative, key.malformed() ? "malformed" : key.scope(), key.scopeValue(), key.target(),
                    rule.isPresent(), rule.map(ParsedRule::deny).orElse(false), rule.map(ParsedRule::reason).orElse(""),
                    rule.map(ParsedRule::code).orElse(0), entry.getValue()));
        }
        return views;
    }

    @PutMapping
    public RuleView put(@RequestBody RuleRequest body, HttpServletRequest request) {
        if (body == null) {
            throw badRequest("request body is required");
        }
        String target = validTarget(body.target());
        if (body.deny() == null) {
            throw badRequest("deny is required (true = disable, false = exempt)");
        }
        String reason = body.reason() == null ? "" : body.reason();
        if (reason.codePointCount(0, reason.length()) > MAX_REASON_CHARS
                || reason.codePoints().anyMatch(Character::isISOControl)) {
            throw badRequest("reason must be at most " + MAX_REASON_CHARS + " characters without control characters");
        }
        int code = body.code() == null ? 0 : body.code();
        if (code < 0 || code > MAX_STATUS_CODE) {
            throw badRequest("code must be 0-" + MAX_STATUS_CODE + " (gRPC status code; 0 = UNAVAILABLE)");
        }

        String relative = scopedKey(body.nodeType(), body.instance()) + target;
        Map<String, Object> value = new LinkedHashMap<>();
        value.put("deny", body.deny());
        if (!reason.isEmpty()) {
            value.put("reason", reason);
        }
        if (code != 0) {
            value.put("code", code);
        }
        String raw;
        try {
            raw = JSON.writeValueAsString(value);
        } catch (JsonProcessingException e) {
            throw new IllegalStateException(e);
        }
        call(() -> {
            store.put(PREFIX + relative, raw);
            return null;
        });
        log.warn("AUDIT killswitch rule set key={} deny={} code={} reason=\"{}\" operator={} remote={}", relative,
                body.deny(), code, reason, operator(request), request.getRemoteAddr());
        ParsedKey key = parseKey(relative);
        return new RuleView(relative, key.scope(), key.scopeValue(), key.target(), true, body.deny(), reason, code, raw);
    }

    @DeleteMapping
    public DeleteResult delete(@RequestParam("key") String key, HttpServletRequest request) {
        String relative = normalizeRelative(key);
        if (relative.isEmpty() || relative.length() > MAX_TARGET_CHARS + 64 || !TARGET_CHARS.matcher(relative).matches()) {
            throw badRequest("key must be a relative rule key such as ClientPlayerChat/SendChat, "
                    + "type/SceneNodeService/* or instance/<uuid>/*");
        }
        long deleted = call(() -> store.delete(PREFIX + relative));
        log.warn("AUDIT killswitch rule removed key={} deleted={} operator={} remote={}", relative, deleted,
                operator(request), request.getRemoteAddr());
        return new DeleteResult(relative, deleted);
    }

    /** 范围段:都不给 = 全部进程;只给 node_type = 一类节点;给 instance 必须同时给 node_type,并在发现里核对。 */
    String scopedKey(String nodeType, String instance) {
        boolean hasType = nodeType != null && !nodeType.isBlank();
        boolean hasInstance = instance != null && !instance.isBlank();
        if (!hasType && !hasInstance) {
            return "";
        }
        if (!hasType) {
            throw badRequest("instance requires node_type, used to check that the node is registered");
        }
        String type = nodeType.strip();
        if (!NODE_TYPE.matcher(type).matches()) {
            throw badRequest("node_type must be an eNodeType name such as SceneNodeService or GateNodeService");
        }
        Set<String> registered = call(() -> store.registeredNodeUuids(type));
        if (!hasInstance) {
            if (registered.isEmpty()) {
                throw badRequest("no node is registered under " + type + ".rpc/; check node_type");
            }
            return TYPE_SCOPE + type + "/";
        }
        String uuid = instance.strip().toLowerCase(Locale.ROOT);
        if (!NODE_UUID.matcher(uuid).matches()) {
            throw badRequest("instance must be a node_uuid (8-4-4-4-12 hex)");
        }
        if (!registered.contains(uuid)) {
            throw badRequest("no " + type + " node with node_uuid " + uuid + " is registered");
        }
        return INSTANCE_SCOPE + uuid + "/";
    }

    /** 目标:{@code *}、{@code 服务/*} 或 {@code 服务/方法}(服务名可带包名)。 */
    static String validTarget(String rawTarget) {
        String target = normalizeRelative(rawTarget);
        if (target.isEmpty() || target.length() > MAX_TARGET_CHARS || !TARGET_CHARS.matcher(target).matches()) {
            throw badRequest("target must be 1-" + MAX_TARGET_CHARS
                    + " characters of [A-Za-z0-9_.$/*-], such as ClientPlayerChat/SendChat, ClientPlayerChat/* or *");
        }
        if (target.equals("*")) {
            return target;
        }
        int slash = target.indexOf('/');
        if (slash <= 0 || slash != target.lastIndexOf('/') || slash == target.length() - 1
                || target.startsWith(TYPE_SCOPE) || target.startsWith(INSTANCE_SCOPE)) {
            throw badRequest("target must be *, Service/* or Service/Method; use node_type / instance for scopes");
        }
        String method = target.substring(slash + 1);
        if (method.contains("*") && !method.equals("*")) {
            throw badRequest("only a whole method may be a wildcard (Service/*)");
        }
        return target;
    }

    /** 去首尾空白与开头的斜杠(与各进程读规则时同一口径)。 */
    static String normalizeRelative(String raw) {
        String text = raw == null ? "" : raw.strip();
        int i = 0;
        while (i < text.length() && text.charAt(i) == '/') {
            i++;
        }
        return text.substring(i);
    }

    static ParsedKey parseKey(String relative) {
        for (String scope : List.of(TYPE_SCOPE, INSTANCE_SCOPE)) {
            if (relative.startsWith(scope)) {
                String rest = relative.substring(scope.length());
                int slash = rest.indexOf('/');
                if (slash <= 0 || slash == rest.length() - 1) {
                    return new ParsedKey(scope.substring(0, scope.length() - 1), "", "");
                }
                return new ParsedKey(scope.substring(0, scope.length() - 1), rest.substring(0, slash),
                        rest.substring(slash + 1));
            }
        }
        return new ParsedKey("all", "", relative);
    }

    static Optional<ParsedRule> parseRule(String raw) {
        String text = raw == null ? "" : raw.strip();
        if (text.isEmpty()) {
            return Optional.of(new ParsedRule(false, "", 0));
        }
        if (text.charAt(0) == '{') {
            return parseJsonRule(text);
        }
        return switch (text.toLowerCase(Locale.ROOT)) {
            case "1", "true", "on", "yes", "deny" -> Optional.of(new ParsedRule(true, "", 0));
            case "0", "false", "off", "no", "allow" -> Optional.of(new ParsedRule(false, "", 0));
            default -> Optional.empty();
        };
    }

    private static Optional<ParsedRule> parseJsonRule(String text) {
        JsonNode node;
        try {
            node = STRICT_JSON.readTree(text);
        } catch (JsonProcessingException e) {
            return Optional.empty();
        }
        if (node == null || !node.isObject()) {
            return Optional.empty();
        }
        JsonNode deny = node.get("deny");
        JsonNode reason = node.get("reason");
        JsonNode code = node.get("code");
        boolean denyOk = deny == null || deny.isNull() || deny.isBoolean();
        boolean reasonOk = reason == null || reason.isNull() || reason.isTextual();
        boolean codeOk = code == null || code.isNull()
                || (code.isIntegralNumber() && code.canConvertToLong() && code.asLong() >= 0 && code.asLong() <= 0xFFFFFFFFL);
        if (!denyOk || !reasonOk || !codeOk) {
            return Optional.empty();
        }
        return Optional.of(new ParsedRule(deny != null && deny.asBoolean(),
                reason == null || reason.isNull() ? "" : reason.asText(),
                code == null || code.isNull() ? 0 : (int) Math.min(code.asLong(), Integer.MAX_VALUE)));
    }

    private static String operator(HttpServletRequest request) {
        String raw = request.getHeader(OPERATOR_HEADER);
        if (raw == null || raw.isBlank()) {
            return "-";
        }
        String text = raw.strip();
        if (text.length() > MAX_OPERATOR_CHARS || !OPERATOR_CHARS.matcher(text).matches()) {
            return "invalid";
        }
        return text;
    }

    private static ResponseStatusException badRequest(String message) {
        return new ResponseStatusException(HttpStatus.BAD_REQUEST, message);
    }

    private static <T> T call(Supplier<T> action) {
        try {
            return action.get();
        } catch (ResponseStatusException e) {
            throw e;
        } catch (RuntimeException e) {
            log.warn("Kill switch admin call failed because etcd is unavailable: {}", e.toString());
            throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "etcd unavailable", e);
        }
    }
}
