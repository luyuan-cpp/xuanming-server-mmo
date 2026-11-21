package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.math.BigInteger;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Set;
import org.apache.shiro.SecurityUtils;
import org.apache.shiro.authz.AuthorizationException;
import org.apache.shiro.subject.Subject;
import org.springframework.beans.BeanWrapperImpl;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Lazy;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.server.ResponseStatusException;

/** 浏览器接缝：身份只取 JeecgBoot 的已认证主体，授权在每次服务端调用时执行。 */
@RestController
@Lazy(false)
@RequestMapping(value = "/gm", produces = "application/json;charset=UTF-8")
public class GmController {
    private static final Set<String> PERMISSIONS = Set.of("gm:zone:read", "gm:player:read", "gm:mail:prepare", "gm:mail:send", "gm:audit:read",
            "gm:currency:add", "gm:currency:deduct", "gm:currency:block", "gm:currency:unblock", "gm:player:level", "gm:pet:grant", "gm:item:grant");
    private static final Set<String> DRAFT_FIELDS = Set.of("clientRequestId", "playerId", "title", "body", "reason", "currencyType", "currencyAmount", "itemConfigId", "itemCount", "validDays");
    private final ObjectMapper json;
    private final GameServices game;
    private final GmCommandJournal journal;
    private final URI gateway;
    private final String gatewayKey;
    private final boolean mailEnabled;
    private final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).followRedirects(HttpClient.Redirect.NEVER).build();

    public GmController(ObjectMapper json, GameServices game, GmCommandJournal journal,
            @Value("${gm.game.gateway-url:http://127.0.0.1:8081}") String gateway,
            @Value("${gm.game.gateway-admin-key:}") String gatewayKey,
            @Value("${gm.game.mail-target:}") String mailTarget,
            @Value("${gm.game.mail-admin-secret:}") String mailSecret) {
        this.json = json; this.game = game; this.journal = journal;
        this.gateway = URI.create(gateway); this.gatewayKey = gatewayKey;
        String host = this.gateway.getHost();
        boolean loopback = "127.0.0.1".equals(host) || "localhost".equals(host) || "[::1]".equals(host);
        if (host == null || this.gateway.getUserInfo() != null || this.gateway.getQuery() != null || this.gateway.getFragment() != null ||
                !("https".equals(this.gateway.getScheme()) || ("http".equals(this.gateway.getScheme()) && loopback))) {
            throw new IllegalArgumentException("GM gateway requires HTTPS or loopback HTTP");
        }
        String trimmedSecret = mailSecret == null ? "" : mailSecret.strip();
        this.mailEnabled = mailTarget != null && !mailTarget.isBlank()
                && trimmedSecret.getBytes(StandardCharsets.UTF_8).length >= 32;
    }

    @GetMapping("/overview")
    public String overview() {
        Subject subject = authenticated();
        if (PERMISSIONS.stream().noneMatch(subject::isPermitted)) throw new AuthorizationException();
        ObjectNode result = json.createObjectNode().put("mailEnabled", mailEnabled);
        ArrayNode currencies = game.currencyTypes();
        result.set("currencies", currencies != null ? currencies : json.createArrayNode());
        result.putArray("zones");
        if (subject.isPermitted("gm:zone:read")) {
            try {
                // 固定只读路径，无浏览器提供的 URL；管理密钥不会返回给浏览器。
                String path = gateway.toString().replaceAll("/+$", "") + "/admin/zones";
                HttpRequest request = HttpRequest.newBuilder(URI.create(path)).timeout(Duration.ofSeconds(5))
                        .header("X-Admin-Key", gatewayKey).GET().build();
                HttpResponse<java.io.InputStream> response = http.send(request, HttpResponse.BodyHandlers.ofInputStream());
                try (var stream = response.body()) {
                    byte[] bytes = stream.readNBytes(262145);
                    if (response.statusCode() != 200 || bytes.length > 262144) throw new IllegalStateException();
                    JsonNode zones = json.readTree(bytes);
                    if (!zones.isArray()) throw new IllegalStateException();
                    result.set("zones", zones); result.put("zonesAvailable", true);
                }
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt(); result.put("zonesAvailable", false);
            } catch (Exception e) { result.put("zonesAvailable", false); }
        }
        return ok(result);
    }

    @GetMapping("/players/{playerId}")
    public String player(@PathVariable String playerId) {
        require("gm:player:read"); decimal(playerId, BigInteger.valueOf(Long.MAX_VALUE), "玩家 ID");
        return ok(game.player(playerId));
    }

    @PostMapping("/mail/drafts")
    public String prepare(@RequestBody String body) {
        String actor = require("gm:mail:prepare");
        if (!mailEnabled) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "邮件服务未配置，暂不能发放");
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(key -> { if (!DRAFT_FIELDS.contains(key)) bad("存在未知字段"); });
        String clientId = text(input, "clientRequestId", 1, 36);
        try { if (!java.util.UUID.fromString(clientId).toString().equals(clientId)) bad("请求号无效"); }
        catch (IllegalArgumentException e) { bad("请求号无效"); }
        String playerId = text(input, "playerId", 1, 19);
        decimal(playerId, BigInteger.valueOf(Long.MAX_VALUE), "玩家 ID");
        ObjectNode request = json.createObjectNode().put("senderKind", 1)
                .put("title", text(input, "title", 1, 128)).put("body", text(input, "body", 0, 4096))
                .put("validDays", integer(input, "validDays", 1, 30));
        ObjectNode attachments = request.putObject("attachments");
        boolean hasCurrency = input.hasNonNull("currencyAmount") && !input.path("currencyAmount").asText().isBlank();
        boolean hasItem = input.hasNonNull("itemConfigId");
        if (hasCurrency) {
            String amount = text(input, "currencyAmount", 1, 19);
            decimal(amount, BigInteger.valueOf(Long.MAX_VALUE), "货币数量");
            int currencyType = input.hasNonNull("currencyType") ? integer(input, "currencyType", 0, Integer.MAX_VALUE) : 0;
            if (!game.supportsCurrency(currencyType)) bad("未知币种");
            attachments.putArray("currencies").addObject().put("currencyType", currencyType).put("amount", amount);
        }
        if (hasItem) attachments.putArray("items").addObject().put("configId", integer(input, "itemConfigId", 1, Integer.MAX_VALUE))
                .put("count", integer(input, "itemCount", 1, 4096));
        if (!hasCurrency && !hasItem) bad("至少填写一种货币或物品附件");
        if (!game.player(playerId).path("found").asBoolean()) bad("未找到目标玩家，请核对玩家 ID");
        return ok(journal.prepare(actor, clientId, playerId, text(input, "reason", 1, 191), request));
    }

    @PostMapping("/mail/commands/{id}/send")
    public String send(@PathVariable String id) {
        String actor = require("gm:mail:send");
        if (!mailEnabled) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "邮件服务未配置，暂不能发放");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.send(id, actor, game::sendMail));
    }

    @GetMapping("/mail/commands")
    public String commands(
            @RequestParam(defaultValue = "1") int page,
            @RequestParam(defaultValue = "50") int pageSize,
            @RequestParam(required = false) String playerId,
            @RequestParam(required = false) String status) {
        require("gm:audit:read");
        return ok(journal.list(page, pageSize, playerId, status));
    }

    @GetMapping("/mail/commands/{id}/audit")
    public String commandAudit(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.auditHistory(id, "MAIL"));
    }

    private Subject authenticated() {
        Subject subject = SecurityUtils.getSubject();
        if (!subject.isAuthenticated() || subject.getPrincipal() == null)
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "请先登录管理员账号");
        return subject;
    }

    private String require(String permission) {
        Subject subject = authenticated(); subject.checkPermission(permission);
        // LoginUser 属于上游，不复制其类型；读取其稳定的 username Bean 属性，并与 GmPlayerCommandController 保持同一安全口径。
        Object username = new BeanWrapperImpl(subject.getPrincipal()).getPropertyValue("username");
        if (!(username instanceof String name) || name.isBlank() || name.getBytes(StandardCharsets.UTF_8).length > 64
                || name.contains("|") || name.codePoints().anyMatch(Character::isISOControl))
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "管理员身份无效或名称过长");
        return name;
    }

    private ObjectNode parse(String body) {
        if (body == null || body.getBytes(StandardCharsets.UTF_8).length > 16384) bad("请求体过大");
        try (JsonParser parser = json.getFactory().createParser(body)) {
            parser.enable(JsonParser.Feature.STRICT_DUPLICATE_DETECTION);
            JsonNode node = json.readTree(parser);
            if (!(node instanceof ObjectNode) || parser.nextToken() != null) bad("请求 JSON 无效");
            return (ObjectNode) node;
        } catch (java.io.IOException e) { bad("请求 JSON 无效"); return null; }
    }
    private static String text(ObjectNode input, String field, int min, int max) {
        JsonNode value = input.get(field);
        if (value == null || !value.isTextual()) { bad(field + " 必须是文本"); }
        String result = value.textValue(); int length = result.codePointCount(0, result.length());
        if (length < min || length > max || (min > 0 && result.isBlank())) bad(field + " 长度无效");
        return result;
    }
    private static int integer(ObjectNode input, String field, int min, int max) {
        JsonNode value = input.get(field);
        if (value == null || !value.isIntegralNumber() || !value.canConvertToInt() || value.intValue() < min || value.intValue() > max) bad(field + " 超出允许范围");
        return value.intValue();
    }
    private static void decimal(String value, BigInteger max, String label) {
        if (!value.matches("[1-9][0-9]{0,18}") || new BigInteger(value).compareTo(max) > 0) bad(label + " 必须是正整数文本");
    }
    private static void bad(String message) { throw new ResponseStatusException(HttpStatus.BAD_REQUEST, message); }
    private String ok(JsonNode result) { return json.createObjectNode().put("success", true).put("code", 200).put("message", "").set("result", result).toString(); }

    @ExceptionHandler(AuthorizationException.class)
    public ResponseEntity<String> forbidden() { return error(403, "没有此 GM 操作权限"); }
    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<String> rejected(ResponseStatusException e) { return error(e.getStatusCode().value(), e.getReason()); }
    @ExceptionHandler(GameServices.CallFailure.class)
    public ResponseEntity<String> upstream(GameServices.CallFailure e) { return error(502, "游戏服务调用未完成：" + e.getCode()); }
    private ResponseEntity<String> error(int code, String message) {
        return ResponseEntity.status(code).body(json.createObjectNode().put("success", false).put("code", code).put("message", message).toString());
    }
}
