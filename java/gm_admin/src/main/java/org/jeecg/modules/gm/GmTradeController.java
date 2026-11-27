package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.util.Set;
import java.util.UUID;
import org.apache.shiro.authz.AuthorizationException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;
import org.springframework.web.server.ResponseStatusException;

/**
 * 交易人工处置页的后端(docs/design/admin-identity-and-dual-control.md §5.6)。
 *
 * <p>流程:发起人看合同(SHOW)→ 填动作与参数,后台先跑一次 DRY_RUN,通过才冻结成草稿 → 批准人看冻结的参数与当场重跑的 DRY_RUN
 * → 批准即执行(EXECUTE 带批准记录)。执行结果由「合同版本 + 动作 + 参数」决定,版本一变 trade 就拒绝,所以批准人看到的就是会执行的。
 * 「两人不同」在流水里判;trade 再核一遍并按批准号幂等。
 */
@RestController
@RequestMapping(value = "/gm/trade", produces = "application/json;charset=UTF-8")
public class GmTradeController {
    static final String READ = "gm:trade:read";
    static final String PREPARE = "gm:trade:prepare";
    static final String APPROVE = "gm:trade:approve";
    private static final Set<String> ACTIONS = Set.of("resume", "reissue", "refund", "partial", "redirect", "void-auction");
    private static final Set<String> DRAFT_FIELDS = Set.of("clientRequestId", "commerceId", "action", "expectVersion", "expectTarget", "reason",
            "parts", "party", "toPlayerId", "linkedCommerceId", "linkedVersion");
    private static final BigInteger MAX_UINT64 = new BigInteger("18446744073709551615");
    private static final int MAX_BODY_BYTES = 16_384;

    private final ObjectMapper json;
    private final GameTradeAdmin trade;
    private final GmCommandJournal journal;

    public GmTradeController(ObjectMapper json, GameTradeAdmin trade, GmCommandJournal journal) {
        this.json = json; this.trade = trade; this.journal = journal;
    }

    @GetMapping("/status")
    public String status() {
        GmAccess.operator(READ);
        return ok(json.createObjectNode().put("enabled", trade.enabled()).put("dualControl", journal.dualControl()));
    }

    @GetMapping("/manual-contracts")
    public String contracts(@RequestParam(defaultValue = "0") String afterUpdatedMs, @RequestParam(defaultValue = "0") String afterCommerceId,
            @RequestParam(defaultValue = "50") int limit) {
        GmAccess.operator(READ);
        cursor(afterUpdatedMs, "翻页游标");
        cursor(afterCommerceId, "翻页游标");
        if (limit < 1 || limit > 100) bad("每页条数须为 1 至 100");
        return ok(trade.list(afterUpdatedMs, afterCommerceId, limit));
    }

    @GetMapping("/manual-contracts/{commerceId}")
    public String show(@PathVariable String commerceId) {
        GmAccess.operator(READ);
        return ok(trade.show(positive(commerceId, "合同号")));
    }

    /** 发起:参数先过一次 DRY_RUN,trade 判定可行才冻结成草稿;不可行把原因原样回给发起人,不建草稿。 */
    @PostMapping("/manual/drafts")
    public String prepare(@RequestBody String body) {
        String actor = GmAccess.operator(PREPARE);
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> { if (!DRAFT_FIELDS.contains(name)) bad("存在未知字段"); });
        String clientRequestId = text(input, "clientRequestId", 36, 36);
        try { if (!UUID.fromString(clientRequestId).toString().equals(clientRequestId)) bad("请求号无效"); }
        catch (IllegalArgumentException invalid) { bad("请求号无效"); }
        String reason = text(input, "reason", 1, 191);
        if (reason.codePoints().anyMatch(Character::isISOControl)) bad("原因不能带换行或控制字符");
        String action = text(input, "action", 1, 16);
        if (!ACTIONS.contains(action)) bad("未知动作");
        ObjectNode frozen = json.createObjectNode()
                .put("commerceId", positive(text(input, "commerceId", 1, 20), "合同号"))
                .put("action", action)
                .put("expectVersion", positive(text(input, "expectVersion", 1, 20), "合同版本"))
                .put("expectTarget", text(input, "expectTarget", 1, 64));
        if (!frozen.path("expectTarget").asText().matches("[A-Za-z0-9._-]{1,64}")) bad("环境名只能是 1~64 个字母、数字或 . _ -");
        // 每个动作只收自己的参数;多给的参数由 trade 判为用法错误,这里只做形状检查。
        if (input.has("parts")) {
            if (!(input.get("parts") instanceof ArrayNode parts) || parts.size() > 16) { bad("缺口最多 16 条"); return null; }
            ArrayNode copy = frozen.putArray("parts");
            for (JsonNode part : parts) {
                if (!part.isTextual() || part.textValue().isBlank() || part.textValue().length() > 256) bad("缺口格式无效");
                copy.add(part.textValue());
            }
        }
        if (input.has("party")) {
            String party = text(input, "party", 1, 8);
            if (!party.equals("seller") && !party.equals("buyer")) bad("改绑方只能是 seller 或 buyer");
            frozen.put("party", party);
        }
        for (String field : new String[] {"toPlayerId", "linkedCommerceId", "linkedVersion"}) {
            if (input.has(field)) frozen.put(field, positive(text(input, field, 1, 20), field));
        }
        ObjectNode plan = trade.dryRun(frozen.deepCopy().put("operator", actor).put("reason", reason));
        if (!"MANUAL_CONTRACT_VERDICT_OK".equals(plan.path("verdict").asText(""))) {
            throw new ResponseStatusException(HttpStatus.CONFLICT, "交易服务判定不可执行：" + refusalOf(plan));
        }
        ObjectNode result = json.createObjectNode();
        result.set("command", journal.prepareTradeManual(actor, clientRequestId, reason, frozen));
        result.set("plan", plan);
        return ok(result);
    }

    /** 批准前重算一遍计划:批准人看到的是当前合同上的结果,不是发起那一刻的。 */
    @PostMapping("/manual/commands/{id}/dry-run")
    public String dryRun(@PathVariable String id) {
        GmAccess.operator(READ);
        ObjectNode command = journal.get(positive(id, "命令号"), "TRADE_MANUAL");
        return ok(trade.dryRun((ObjectNode) command.path("request")));
    }

    @PostMapping("/manual/commands/{id}/send")
    public String send(@PathVariable String id) {
        String actor = GmAccess.operator(APPROVE);
        if (!trade.enabled()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "交易管理通道未配置");
        return ok(journal.sendTradeManual(positive(id, "命令号"), actor, trade::execute));
    }

    /** 有批准权限的人可以驳回任何人的待批申请;只有发起权限的人只能撤回自己发起的那一条。 */
    @PostMapping("/manual/commands/{id}/decline")
    public String decline(@PathVariable String id, @RequestBody String body) {
        boolean mayApprove = GmAccess.authenticated().isPermitted(APPROVE);
        String actor = GmAccess.operator(mayApprove ? APPROVE : PREPARE);
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> { if (!name.equals("reason")) bad("存在未知字段"); });
        return ok(journal.decline(positive(id, "命令号"), actor, text(input, "reason", 1, 191), "TRADE_MANUAL", !mayApprove));
    }

    @GetMapping("/manual/commands")
    public String commands(@RequestParam(defaultValue = "1") int page, @RequestParam(defaultValue = "50") int pageSize,
            @RequestParam(required = false) String status) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.listTradeManual(page, pageSize, status));
    }

    @GetMapping("/manual/commands/{id}")
    public String command(@PathVariable String id) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.get(positive(id, "命令号"), "TRADE_MANUAL"));
    }

    @GetMapping("/manual/commands/{id}/audit")
    public String audit(@PathVariable String id) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.auditHistory(positive(id, "命令号"), "TRADE_MANUAL"));
    }

    private static String refusalOf(ObjectNode reply) {
        String verdict = reply.path("verdict").asText("").replace("MANUAL_CONTRACT_VERDICT_", "");
        String refusal = reply.path("refusal").asText("");
        return refusal.isEmpty() ? verdict : verdict + " " + refusal;
    }

    private static void cursor(String value, String label) {
        if (!value.equals("0")) positive(value, label);
    }

    private static String positive(String value, String label) {
        if (value == null || !value.matches("[1-9][0-9]{0,19}") || new BigInteger(value).compareTo(MAX_UINT64) > 0)
            bad(label + " 必须是正整数文本");
        return value;
    }

    private ObjectNode parse(String body) {
        if (body == null || body.getBytes(StandardCharsets.UTF_8).length > MAX_BODY_BYTES) bad("请求体过大");
        try (JsonParser parser = json.getFactory().createParser(body)) {
            parser.enable(JsonParser.Feature.STRICT_DUPLICATE_DETECTION);
            JsonNode node = json.readTree(parser);
            if (!(node instanceof ObjectNode object) || parser.nextToken() != null) { bad("请求 JSON 无效"); return null; }
            return object;
        } catch (java.io.IOException invalid) { bad("请求 JSON 无效"); return null; }
    }

    private static String text(ObjectNode input, String field, int min, int max) {
        JsonNode value = input.get(field);
        if (value == null || !value.isTextual()) bad(field + " 必须是文本");
        String result = value.textValue();
        int length = result.codePointCount(0, result.length());
        if (length < min || length > max || result.isBlank()) bad(field + " 长度无效");
        return result;
    }

    private static void bad(String message) { throw new ResponseStatusException(HttpStatus.BAD_REQUEST, message); }

    private String ok(JsonNode result) {
        return json.createObjectNode().put("success", true).put("code", 200).put("message", "").set("result", result).toString();
    }

    @ExceptionHandler(AuthorizationException.class)
    public ResponseEntity<String> forbidden() { return error(403, "没有此 GM 操作权限"); }
    @ExceptionHandler(MethodArgumentTypeMismatchException.class)
    public ResponseEntity<String> badParameter() { return error(400, "请求参数格式无效"); }
    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<String> rejected(ResponseStatusException e) { return error(e.getStatusCode().value(), e.getReason()); }
    @ExceptionHandler(GameServices.CallFailure.class)
    public ResponseEntity<String> upstream(GameServices.CallFailure e) { return error(502, "交易服务调用未完成：" + e.getCode()); }

    private ResponseEntity<String> error(int code, String message) {
        return ResponseEntity.status(code).body(json.createObjectNode().put("success", false).put("code", code).put("message", message).toString());
    }
}
