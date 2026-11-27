package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.function.BiFunction;
import java.util.function.BooleanSupplier;
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
 * 「运营处置」页的后端(docs/design/admin-identity-and-dual-control.md §8 批 4、批 5):
 * 甲级管理动作统一经流水的 OPS 类别发起、由另一人批准后放行。
 *
 * <p>目录 {@link #COMMANDS} 是唯一的白名单:下游(target)、命令、允许的正文字段,以及由服务端填的字段(操作人、原因)。
 * 权限按下游分:gm:&lt;target&gt;:read / prepare / approve。「两人不同」只在流水里判;下游再核一遍批准记录,并按批准号判重。
 */
@RestController
@RequestMapping(value = "/gm/ops", produces = "application/json;charset=UTF-8")
public class GmOpsController {
    /** 下游名单:迁移 9 起按这里给每个下游建三个权限串。 */
    static final List<String> TARGETS = List.of("mall");
    private static final int MAX_BODY_BYTES = 16_384;
    private static final BigInteger MAX_UINT64 = new BigInteger("18446744073709551615");

    /**
     * 一条可发起的命令。fields 是发起人能填的正文字段;reasonField / operatorField 不为 null 时,服务端把流水的原因与操作人填进这个字段,
     * 发起人不能自己填。
     */
    record Command(String target, String name, Set<String> fields, String reasonField, String operatorField) { }

    static final Map<String, Command> COMMANDS = Map.ofEntries(
            entry(new Command("mall", "manual/orders/resolve", Set.of("orderId", "action"), "reason", null)),
            entry(new Command("mall", "manual/refunds/resolve", Set.of("orderId", "channel", "paymentTransactionId", "action", "externalRefundId"), "reason", null)),
            entry(new Command("mall", "manual/consignments/resolve", Set.of("listingId", "action"), "reason", null)),
            entry(new Command("mall", "manual/incomes/dispute", Set.of("orderId", "action", "amountFen", "caseId"), "reason", null)),
            entry(new Command("mall", "skus/create", Set.of("skuId", "terms", "stock"), null, null)),
            entry(new Command("mall", "skus/update", Set.of("skuId", "expectedRevision", "terms"), null, null)),
            entry(new Command("mall", "skus/stock", Set.of("skuId", "expectedRevision", "delta"), null, null)));

    private static Map.Entry<String, Command> entry(Command command) { return Map.entry(command.target() + "/" + command.name(), command); }

    private final ObjectMapper json;
    private final GmCommandJournal journal;
    private final GameMallAdmin mall;
    private final Map<String, BiFunction<ObjectNode, ObjectNode, ObjectNode>> senders;
    private final Map<String, BooleanSupplier> enabled;

    public GmOpsController(ObjectMapper json, GmCommandJournal journal, GameMallAdmin mall) {
        this.json = json; this.journal = journal; this.mall = mall;
        this.senders = Map.of("mall", mall::execute);
        this.enabled = Map.of("mall", mall::enabled);
    }

    @GetMapping("/status")
    public String status() {
        GmAccess.authenticated();
        ObjectNode result = json.createObjectNode().put("dualControl", journal.dualControl());
        ObjectNode targets = result.putObject("targets");
        enabled.forEach((target, on) -> targets.put(target, on.getAsBoolean()));
        return ok(result);
    }

    /** 商城人工待办(订单 / 退款 / 寄售三类,各自翻页)。 */
    @GetMapping("/mall/manual")
    public String mallManual(@RequestParam(defaultValue = "") String afterOrderId, @RequestParam(defaultValue = "") String afterListingId,
            @RequestParam(defaultValue = "") String afterRefundCursor, @RequestParam(defaultValue = "50") int limit) {
        GmAccess.operator(permission("mall", "read"));
        if (limit < 1 || limit > 100) bad("每页条数须为 1 至 100");
        for (String cursor : List.of(afterOrderId, afterListingId, afterRefundCursor))
            if (cursor.length() > 128 || !cursor.matches("[A-Za-z0-9:_-]*")) bad("翻页游标无效");
        return ok(mall.read("manual/list", json.createObjectNode().put("afterOrderId", afterOrderId).put("afterListingId", afterListingId)
                .put("afterRefundCursor", afterRefundCursor).put("limit", limit)));
    }

    @GetMapping("/mall/orders/{orderId}")
    public String mallOrder(@PathVariable String orderId) {
        GmAccess.operator(permission("mall", "read"));
        if (!orderId.matches("[A-Za-z0-9_-]{1,64}")) bad("订单号无效");
        return ok(mall.read("order-view", json.createObjectNode().put("orderId", orderId)));
    }

    @GetMapping("/mall/skus")
    public String mallSkus(@RequestParam(defaultValue = "0") long afterSkuId, @RequestParam(defaultValue = "50") int limit) {
        GmAccess.operator(permission("mall", "read"));
        if (afterSkuId < 0 || limit < 1 || limit > 100) bad("翻页参数无效");
        return ok(mall.read("skus/list", json.createObjectNode().put("afterSkuId", afterSkuId).put("limit", limit)));
    }

    /** 发起:命令与正文字段按目录核对,服务端字段由这里填,流水再补请求号;草稿冻结后不可改。 */
    @PostMapping("/drafts")
    public String prepare(@RequestBody String body) {
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> {
            if (!Set.of("clientRequestId", "target", "command", "body", "reason").contains(name)) bad("存在未知字段");
        });
        Command command = COMMANDS.get(text(input, "target", 1, 32) + "/" + text(input, "command", 1, 64));
        if (command == null) bad("未知的运营处置命令");
        String actor = GmAccess.operator(permission(command.target(), "prepare"));
        String clientRequestId = text(input, "clientRequestId", 36, 36);
        try { if (!UUID.fromString(clientRequestId).toString().equals(clientRequestId)) bad("请求号无效"); }
        catch (IllegalArgumentException invalid) { bad("请求号无效"); }
        String reason = text(input, "reason", 1, 191);
        if (reason.codePoints().anyMatch(Character::isISOControl)) bad("原因不能带换行或控制字符");
        if (!(input.get("body") instanceof ObjectNode fields)) { bad("body 必须是对象"); return null; }
        fields.fieldNames().forEachRemaining(name -> { if (!command.fields().contains(name)) bad("命令不接受字段 " + name); });
        ObjectNode frozenBody = fields.deepCopy();
        if (command.reasonField() != null) frozenBody.put(command.reasonField(), reason);
        if (command.operatorField() != null) frozenBody.put(command.operatorField(), actor);
        ObjectNode frozen = json.createObjectNode().put("target", command.target()).put("command", command.name());
        frozen.set("body", frozenBody);
        return ok(journal.prepareOps(actor, clientRequestId, reason, frozen));
    }

    @PostMapping("/commands/{id}/send")
    public String send(@PathVariable String id) {
        String target = targetOf(id);
        String actor = GmAccess.operator(permission(target, "approve"));
        if (!enabled.get(target).getAsBoolean()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "该下游的管理通道未配置");
        return ok(journal.sendOps(id, actor, senders.get(target)));
    }

    /** 有批准权限的人可以驳回任何人的待批申请;只有发起权限的人只能撤回自己发起的那一条。 */
    @PostMapping("/commands/{id}/decline")
    public String decline(@PathVariable String id, @RequestBody String body) {
        String target = targetOf(id);
        boolean mayApprove = GmAccess.authenticated().isPermitted(permission(target, "approve"));
        String actor = GmAccess.operator(permission(target, mayApprove ? "approve" : "prepare"));
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> { if (!name.equals("reason")) bad("存在未知字段"); });
        return ok(journal.decline(id, actor, text(input, "reason", 1, 191), "OPS", !mayApprove));
    }

    @GetMapping("/commands")
    public String commands(@RequestParam(defaultValue = "1") int page, @RequestParam(defaultValue = "50") int pageSize,
            @RequestParam(required = false) String status) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.listOps(page, pageSize, status));
    }

    @GetMapping("/commands/{id}")
    public String command(@PathVariable String id) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.get(positive(id), "OPS"));
    }

    @GetMapping("/commands/{id}/audit")
    public String audit(@PathVariable String id) {
        GmAccess.operator("gm:audit:read");
        return ok(journal.auditHistory(positive(id), "OPS"));
    }

    static String permission(String target, String action) { return "gm:" + target + ":" + action; }

    /** 下游取自流水里冻结的请求;目录里已经没有的下游一律拒绝。 */
    private String targetOf(String id) {
        GmAccess.authenticated();
        String target = journal.get(positive(id), "OPS").path("request").path("target").asText("");
        if (!senders.containsKey(target)) throw new ResponseStatusException(HttpStatus.CONFLICT, "这条申请的下游已不受支持");
        return target;
    }

    private static String positive(String value) {
        if (value == null || !value.matches("[1-9][0-9]{0,19}") || new BigInteger(value).compareTo(MAX_UINT64) > 0) bad("命令号必须是正整数文本");
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
    public ResponseEntity<String> upstream(GameServices.CallFailure e) { return error(502, e.getMessage()); }

    private ResponseEntity<String> error(int code, String message) {
        return ResponseEntity.status(code).body(json.createObjectNode().put("success", false).put("code", code).put("message", message).toString());
    }
}
