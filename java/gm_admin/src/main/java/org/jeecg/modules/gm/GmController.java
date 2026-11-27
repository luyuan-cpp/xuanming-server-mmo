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
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.regex.Pattern;
import org.apache.shiro.SecurityUtils;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.apache.shiro.authz.AuthorizationException;
import org.apache.shiro.subject.Subject;
import org.springframework.beans.BeanWrapperImpl;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Lazy;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;
import org.springframework.web.server.ResponseStatusException;

/** 浏览器接缝：身份只取 JeecgBoot 的已认证主体，授权在每次服务端调用时执行。 */
@RestController
@Lazy(false)
@RequestMapping(value = "/gm", produces = "application/json;charset=UTF-8")
public class GmController {
    private static final Logger LOG = LoggerFactory.getLogger(GmController.class);
    /** 网关管理面的写操作(乙级,设计 §3):单人可做,要个人身份与留痕。只转发这张白名单里的方法与路径。 */
    static final String GATEWAY_WRITE = "gm:gateway:write";
    private static final String ID = "[1-9][0-9]{0,18}";
    private static final List<Pattern> GATEWAY_WRITES = List.of(
            Pattern.compile("POST /admin/zones"),
            Pattern.compile("(PUT|DELETE) /admin/zones/" + ID),
            Pattern.compile("POST /admin/zones/" + ID + "/(maintenance|open|create-role/close|create-role/open)"),
            Pattern.compile("PUT /admin/zones/" + ID + "/role-capacity"),
            Pattern.compile("POST /admin/whitelist"),
            Pattern.compile("DELETE /admin/whitelist/" + ID + "/" + ID),
            Pattern.compile("POST /admin/announcements"),
            Pattern.compile("DELETE /admin/announcements/" + ID),
            Pattern.compile("PUT /admin/killswitch"),
            Pattern.compile("DELETE /admin/killswitch\\?key=[A-Za-z0-9_.$*%-]{1,320}"));
    private static final Set<String> PERMISSIONS = Set.of("gm:zone:read", "gm:player:read", "gm:mail:prepare", "gm:mail:send",
            "gm:system-mail:prepare", "gm:system-mail:send", "gm:audit:read",
            "gm:currency:add", "gm:currency:deduct", "gm:currency:block", "gm:currency:unblock", "gm:player:level", "gm:pet:grant", "gm:item:grant",
            GATEWAY_WRITE);
    private static final Set<String> DRAFT_FIELDS = Set.of("clientRequestId", "playerId", "title", "body", "reason",
            "currencyType", "currencyAmount", "itemConfigId", "itemCount", "currencies", "items", "validDays");
    private static final Set<String> SYSTEM_MAIL_FIELDS = Set.of("clientRequestId", "title", "body", "reason", "validDays");
    private static final Set<String> CAMPAIGN_FIELDS = Set.of("clientRequestId", "title", "body", "reason",
            "currencies", "items", "recipientPlayerIds", "validDays");
    private static final Set<String> CURRENCY_ENTRY_FIELDS = Set.of("currencyType", "amount");
    private static final Set<String> ITEM_ENTRY_FIELDS = Set.of("configId", "count");
    private static final int DEFAULT_MAX_BODY_BYTES = 16_384;
    private static final int CAMPAIGN_MAX_BODY_BYTES = 512 * 1024;
    private static final int MAX_CURRENCIES = 4;
    private static final int MAX_ITEMS = 16;
    private static final int MAX_TOTAL_ITEM_COUNT = 4096;
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
        // dualControl 只是给页面看的：开着时页面把「发送」显示成「批准并发送」，并把自己发起的申请标成不能自批。规则本身在流水里。
        ObjectNode result = json.createObjectNode().put("mailEnabled", mailEnabled).put("dualControl", journal.dualControl());
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

    /**
     * 网关管理面写操作的唯一人用入口(docs/design/admin-identity-and-dual-control.md §8 批 6):网关口令留在服务端,
     * X-Admin-Operator 取登录会话里的操作人,网关据此要求合规的操作人头并打 AUDIT 行;这里另打一行进后台日志。
     * 回包原样带回网关的状态码与正文(最多 256 KiB)。
     */
    @PostMapping("/gateway/admin")
    public String gatewayAdmin(@RequestBody String body) {
        String actor = require(GATEWAY_WRITE);
        ObjectNode input = parse(body, DEFAULT_MAX_BODY_BYTES);
        input.fieldNames().forEachRemaining(key -> { if (!Set.of("method", "path", "body").contains(key)) bad("存在未知字段"); });
        String method = text(input, "method", 3, 6);
        String path = text(input, "path", 1, 400);
        if (GATEWAY_WRITES.stream().noneMatch(rule -> rule.matcher(method + " " + path).matches())) bad("不支持的网关管理操作");
        JsonNode payload = input.get("body");
        if (payload != null && !payload.isObject()) bad("body 必须是对象");
        if (!actor.matches("[A-Za-z0-9._@-]{1,64}")) bad("当前管理员名称不能作为网关操作人");
        HttpRequest.Builder request = HttpRequest.newBuilder(URI.create(gateway.toString().replaceAll("/+$", "") + path))
                .timeout(Duration.ofSeconds(8)).header("X-Admin-Key", gatewayKey).header("X-Admin-Operator", actor)
                .header("Content-Type", "application/json")
                .method(method, payload == null ? HttpRequest.BodyPublishers.noBody()
                        : HttpRequest.BodyPublishers.ofString(payload.toString(), StandardCharsets.UTF_8));
        int status;
        String reply;
        try {
            HttpResponse<java.io.InputStream> response = http.send(request.build(), HttpResponse.BodyHandlers.ofInputStream());
            try (var stream = response.body()) {
                byte[] bytes = stream.readNBytes(262145);
                if (bytes.length > 262144) throw new java.io.IOException("gateway admin response too large");
                status = response.statusCode();
                reply = new String(bytes, StandardCharsets.UTF_8);
            }
        } catch (InterruptedException interrupted) {
            Thread.currentThread().interrupt();
            throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "网关管理请求已取消，请刷新核对结果");
        } catch (java.io.IOException | RuntimeException unavailable) {
            LOG.warn("GM gateway admin write outcome unknown operator={} method={} path={}", actor, method, path);
            throw new ResponseStatusException(HttpStatus.BAD_GATEWAY, "网关管理请求结果未知，请刷新核对后再决定是否重做");
        }
        LOG.info("GM gateway admin write operator={} method={} path={} status={}", actor, method, path, status);
        ObjectNode result = json.createObjectNode().put("status", status);
        try { result.set("body", reply.isEmpty() ? json.nullNode() : json.readTree(reply)); }
        catch (java.io.IOException notJson) { result.put("body", reply); }
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
        requireMailEnabled();
        ObjectNode input = parse(body, DEFAULT_MAX_BODY_BYTES);
        input.fieldNames().forEachRemaining(key -> { if (!DRAFT_FIELDS.contains(key)) bad("存在未知字段"); });
        String clientId = uuid(input);
        String playerId = text(input, "playerId", 1, 19);
        decimal(playerId, BigInteger.valueOf(Long.MAX_VALUE), "玩家 ID");
        ObjectNode request = json.createObjectNode().put("senderKind", 1)
                .put("title", text(input, "title", 1, 128)).put("body", text(input, "body", 0, 4096))
                .put("validDays", integer(input, "validDays", 1, 30));
        request.set("attachments", buildAttachments(input, true));
        if (!game.player(playerId).path("found").asBoolean()) bad("未找到目标玩家，请核对玩家 ID");
        return ok(journal.prepare(actor, clientId, playerId, text(input, "reason", 1, 191), request));
    }

    @PostMapping("/mail/commands/{id}/send")
    public String send(@PathVariable String id) {
        String actor = require("gm:mail:send");
        requireMailEnabled();
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.send(id, actor, game::sendMail));
    }

    @PostMapping("/mail/commands/{id}/decline")
    public String decline(@PathVariable String id, @RequestBody String body) {
        return decline(id, body, "MAIL", "gm:mail:prepare", "gm:mail:send");
    }

    /** 驳回要有放行权限；只有发起权限的人只能撤回自己发起的申请。 */
    private String decline(String id, String body, String kind, String preparePermission, String releasePermission) {
        boolean mayRelease = authenticated().isPermitted(releasePermission);
        String actor = require(mayRelease ? releasePermission : preparePermission);
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        ObjectNode input = parse(body, DEFAULT_MAX_BODY_BYTES);
        input.fieldNames().forEachRemaining(key -> { if (!key.equals("reason")) bad("存在未知字段"); });
        return ok(journal.decline(id, actor, text(input, "reason", 1, 191), kind, !mayRelease));
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

    @GetMapping("/mail/commands/{id}")
    public String command(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.get(id, "MAIL"));
    }

    @GetMapping("/mail/commands/{id}/audit")
    public String commandAudit(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.auditHistory(id, "MAIL"));
    }

    @PostMapping("/system-mail/drafts")
    public String prepareSystemMail(@RequestBody String body) {
        String actor = require("gm:system-mail:prepare");
        requireMailEnabled();
        ObjectNode input = parse(body, DEFAULT_MAX_BODY_BYTES);
        input.fieldNames().forEachRemaining(key -> { if (!SYSTEM_MAIL_FIELDS.contains(key)) bad("存在未知字段"); });
        String clientId = uuid(input);
        ObjectNode request = json.createObjectNode().put("senderKind", 1)
                .put("title", text(input, "title", 1, 128)).put("body", text(input, "body", 0, 4096))
                .put("validDays", integer(input, "validDays", 1, 30));
        return ok(journal.prepareSystemMail(actor, clientId, text(input, "reason", 1, 191), request));
    }

    @PostMapping("/system-mail/commands/{id}/send")
    public String sendSystemMail(@PathVariable String id) {
        String actor = require("gm:system-mail:send");
        requireMailEnabled();
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.sendSystemMail(id, actor, game::sendSystemMail));
    }

    @PostMapping("/system-mail/commands/{id}/decline")
    public String declineSystemMail(@PathVariable String id, @RequestBody String body) {
        return decline(id, body, "SYSTEM_MAIL", "gm:system-mail:prepare", "gm:system-mail:send");
    }

    @GetMapping("/system-mail/commands")
    public String systemMailCommands(
            @RequestParam(defaultValue = "1") int page,
            @RequestParam(defaultValue = "50") int pageSize,
            @RequestParam(required = false) String status) {
        require("gm:audit:read");
        return ok(journal.listSystemMails(page, pageSize, status));
    }

    @GetMapping("/system-mail/commands/{id}")
    public String systemMailCommand(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.get(id, "SYSTEM_MAIL"));
    }

    @GetMapping("/system-mail/commands/{id}/audit")
    public String systemMailAudit(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.auditHistory(id, "SYSTEM_MAIL"));
    }

    @PostMapping("/mail-campaigns/drafts")
    public String prepareMailCampaign(@RequestBody String body) {
        String actor = require("gm:system-mail:prepare");
        requireMailEnabled();
        ObjectNode input = parse(body, CAMPAIGN_MAX_BODY_BYTES);
        input.fieldNames().forEachRemaining(key -> { if (!CAMPAIGN_FIELDS.contains(key)) bad("存在未知字段"); });
        String clientId = uuid(input);
        JsonNode rawRecipients = input.get("recipientPlayerIds");
        if (rawRecipients == null || !rawRecipients.isArray() || rawRecipients.isEmpty()
                || rawRecipients.size() > MailCampaignManifest.MAX_WEB_RECIPIENTS * 2) {
            bad("收件人名单必须是非空玩家 ID 数组，且去重后不超过 " + MailCampaignManifest.MAX_WEB_RECIPIENTS + " 人");
        }
        List<String> recipientIds = new ArrayList<>(rawRecipients.size());
        for (JsonNode item : rawRecipients) {
            if (!item.isTextual()) bad("收件人玩家 ID 必须是十进制字符串");
            recipientIds.add(item.textValue());
        }
        MailCampaignManifest.Plan plan;
        try { plan = MailCampaignManifest.build(recipientIds); }
        catch (IllegalArgumentException e) { bad(e.getMessage()); return null; }

        ObjectNode mail = json.createObjectNode().put("senderKind", 1)
                .put("title", text(input, "title", 1, 128)).put("body", text(input, "body", 0, 4096))
                .put("validDays", integer(input, "validDays", 1, 30))
                .put("expectedRecipientCount", Integer.toString(plan.recipientCount()))
                .put("expectedShardCount", plan.shards().size())
                .put("manifestHash", plan.manifestHash());
        mail.set("attachments", buildAttachments(input, false));
        ObjectNode campaign = json.createObjectNode();
        campaign.set("mail", mail);
        ArrayNode shardsNode = campaign.putArray("shards");
        for (List<String> shard : plan.shards()) {
            ArrayNode shardArray = shardsNode.addArray();
            shard.forEach(shardArray::add);
        }
        return ok(journal.prepareMailCampaign(actor, clientId, text(input, "reason", 1, 191), campaign));
    }

    @PostMapping("/mail-campaigns/commands/{id}/send")
    public String sendMailCampaign(@PathVariable String id) {
        String actor = require("gm:system-mail:send");
        requireMailEnabled();
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.sendMailCampaign(id, actor, game::sendMailCampaign));
    }

    @PostMapping("/mail-campaigns/commands/{id}/decline")
    public String declineMailCampaign(@PathVariable String id, @RequestBody String body) {
        return decline(id, body, "MAIL_CAMPAIGN", "gm:system-mail:prepare", "gm:system-mail:send");
    }

    @GetMapping("/mail-campaigns/commands")
    public String mailCampaignCommands(
            @RequestParam(defaultValue = "1") int page,
            @RequestParam(defaultValue = "50") int pageSize,
            @RequestParam(required = false) String status) {
        require("gm:audit:read");
        return ok(journal.listMailCampaigns(page, pageSize, status));
    }

    @GetMapping("/mail-campaigns/commands/{id}")
    public String mailCampaignCommand(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.get(id, "MAIL_CAMPAIGN"));
    }

    @GetMapping("/mail-campaigns/commands/{id}/audit")
    public String mailCampaignAudit(@PathVariable String id) {
        require("gm:audit:read");
        decimal(id, BigInteger.valueOf(Long.MAX_VALUE), "命令 ID");
        return ok(journal.auditHistory(id, "MAIL_CAMPAIGN"));
    }

    private void requireMailEnabled() {
        if (!mailEnabled) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "邮件服务未配置，暂不能发放");
    }

    private ObjectNode buildAttachments(ObjectNode input, boolean allowLegacyFlat) {
        ObjectNode attachments = json.createObjectNode();
        boolean hasCurrencyArray = input.hasNonNull("currencies");
        boolean hasItemArray = input.hasNonNull("items");
        boolean hasFlatCurrency = allowLegacyFlat && input.hasNonNull("currencyAmount") && !input.path("currencyAmount").asText().isBlank();
        boolean hasFlatItem = allowLegacyFlat && (input.hasNonNull("itemConfigId") || input.hasNonNull("itemCount"));
        if (hasCurrencyArray && (hasFlatCurrency || (allowLegacyFlat && input.hasNonNull("currencyType")))) {
            bad("货币附件不能同时混用数组和单字段格式");
        }
        if (hasItemArray && hasFlatItem) bad("物品附件不能同时混用数组和单字段格式");

        int currencyCount = 0;
        if (hasCurrencyArray) {
            JsonNode node = input.get("currencies");
            if (!node.isArray() || node.size() > MAX_CURRENCIES) bad("货币附件数组格式非法或超过 4 种上限");
            Set<Integer> seen = new HashSet<>();
            ArrayNode out = null;
            for (JsonNode entry : node) {
                if (!(entry instanceof ObjectNode)) bad("货币附件项必须是对象");
                ObjectNode obj = (ObjectNode) entry;
                obj.fieldNames().forEachRemaining(k -> { if (!CURRENCY_ENTRY_FIELDS.contains(k)) bad("货币附件存在未知字段"); });
                int currencyType = integer(obj, "currencyType", 0, Integer.MAX_VALUE);
                if (!game.supportsCurrency(currencyType)) bad("未知币种");
                if (!seen.add(currencyType)) bad("货币附件不能包含重复币种");
                String amount = text(obj, "amount", 1, 19);
                decimal(amount, BigInteger.valueOf(Long.MAX_VALUE), "货币数量");
                if (out == null) out = attachments.putArray("currencies");
                out.addObject().put("currencyType", currencyType).put("amount", amount);
                currencyCount++;
            }
        } else if (hasFlatCurrency) {
            String amount = text(input, "currencyAmount", 1, 19);
            decimal(amount, BigInteger.valueOf(Long.MAX_VALUE), "货币数量");
            int currencyType = input.hasNonNull("currencyType") ? integer(input, "currencyType", 0, Integer.MAX_VALUE) : 0;
            if (!game.supportsCurrency(currencyType)) bad("未知币种");
            attachments.putArray("currencies").addObject().put("currencyType", currencyType).put("amount", amount);
            currencyCount = 1;
        }

        int itemEntries = 0;
        int totalItems = 0;
        if (hasItemArray) {
            JsonNode node = input.get("items");
            if (!node.isArray() || node.size() > MAX_ITEMS) bad("物品附件数组格式非法或超过 16 种上限");
            Set<Integer> seen = new HashSet<>();
            ArrayNode out = null;
            for (JsonNode entry : node) {
                if (!(entry instanceof ObjectNode)) bad("物品附件项必须是对象");
                ObjectNode obj = (ObjectNode) entry;
                obj.fieldNames().forEachRemaining(k -> { if (!ITEM_ENTRY_FIELDS.contains(k)) bad("物品附件存在未知字段"); });
                int configId = integer(obj, "configId", 1, Integer.MAX_VALUE);
                if (!seen.add(configId)) bad("物品附件不能包含重复配置 ID");
                int count = integer(obj, "count", 1, MAX_TOTAL_ITEM_COUNT);
                totalItems += count;
                if (totalItems > MAX_TOTAL_ITEM_COUNT) bad("单封邮件物品总数不能超过 4096");
                if (out == null) out = attachments.putArray("items");
                out.addObject().put("configId", configId).put("count", count);
                itemEntries++;
            }
        } else if (hasFlatItem) {
            attachments.putArray("items").addObject().put("configId", integer(input, "itemConfigId", 1, Integer.MAX_VALUE))
                    .put("count", integer(input, "itemCount", 1, MAX_TOTAL_ITEM_COUNT));
            itemEntries = 1;
        }

        if (currencyCount == 0 && itemEntries == 0) bad("至少填写一种货币或物品附件");
        return attachments;
    }

    private static String uuid(ObjectNode input) {
        String clientId = text(input, "clientRequestId", 1, 36);
        try { if (!java.util.UUID.fromString(clientId).toString().equals(clientId)) bad("请求号无效"); }
        catch (IllegalArgumentException e) { bad("请求号无效"); }
        return clientId;
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

    private ObjectNode parse(String body, int maxBytes) {
        if (body == null || body.getBytes(StandardCharsets.UTF_8).length > maxBytes) bad("请求体过大");
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
    @ExceptionHandler(MethodArgumentTypeMismatchException.class)
    public ResponseEntity<String> badParameter() { return error(400, "请求参数格式无效"); }
    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<String> rejected(ResponseStatusException e) { return error(e.getStatusCode().value(), e.getReason()); }
    @ExceptionHandler(GameServices.CallFailure.class)
    public ResponseEntity<String> upstream(GameServices.CallFailure e) { return error(502, "游戏服务调用未完成：" + e.getCode()); }
    private ResponseEntity<String> error(int code, String message) {
        return ResponseEntity.status(code).body(json.createObjectNode().put("success", false).put("code", code).put("message", message).toString());
    }
}
