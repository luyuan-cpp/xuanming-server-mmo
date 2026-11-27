package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.function.BiFunction;
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
 * <p>目录 {@link #COMMANDS} 是唯一的白名单:下游(target)、命令、允许的正文字段、由服务端填的字段(操作人、原因、冻结时刻、固定模式)、
 * 请求号字段(请求号就是批准号)、能不能沿用原申请重试,以及有没有不进流水的预览(演练 / 试算)。
 * 权限按下游分:gm:&lt;target&gt;:read / prepare / approve。「两人不同」只在流水里判;下游再核一遍批准记录。
 */
@RestController
@RequestMapping(value = "/gm/ops", produces = "application/json;charset=UTF-8")
public class GmOpsController {
    /** 下游名单:迁移 9、10 按这里给每个下游建三个权限串。 */
    static final List<String> TARGETS = List.of("mall", "account", "recall", "inbox", "rank", "match");
    private static final int MAX_BODY_BYTES = 16_384;
    private static final BigInteger MAX_UINT64 = new BigInteger("18446744073709551615");
    /** filled 里的值:这三个占位由服务端换成申请的原因、操作人与冻结时刻,其余原样写进正文(如固定的模式)。 */
    static final String REASON = "$reason";
    static final String OPERATOR = "$operator";
    static final String NOW_MS = "$nowMs";

    /**
     * 一条可发起的命令。
     *
     * @param method        gRPC 完整方法名;商城走 HTTP,为 null
     * @param fields        发起人能填的正文字段(protobuf JSON 名)
     * @param filled        服务端填的字段;发起人不能自己填
     * @param requestIdField 请求号字段:放行时填成批准号(下游按它判重);没有请求号的命令为 null
     * @param retryable     结果未知后能否沿用原申请重试:下游按批准号判重、或重放天然收敛才是 true
     * @param preview       不为 null 时可以预览:在 filled 之上再覆盖这些字段(如 dryRun=true),直接调用、不进流水
     */
    record Command(String target, String name, String method, Set<String> fields, Map<String, Object> filled,
            String requestIdField, boolean retryable, Map<String, Object> preview) { }

    static final Map<String, Command> COMMANDS = catalog(
            // 商城(HTTP,GameMallAdmin):按 (主体, 请求号) 幂等,请求号由 GameMallAdmin 填。
            new Command("mall", "manual/orders/resolve", null, Set.of("orderId", "action"), Map.of("reason", REASON), null, true, null),
            new Command("mall", "manual/refunds/resolve", null, Set.of("orderId", "channel", "paymentTransactionId", "action", "externalRefundId"),
                    Map.of("reason", REASON), null, true, null),
            new Command("mall", "manual/consignments/resolve", null, Set.of("listingId", "action"), Map.of("reason", REASON), null, true, null),
            new Command("mall", "manual/incomes/dispute", null, Set.of("orderId", "action", "amountFen", "caseId"), Map.of("reason", REASON), null, true, null),
            new Command("mall", "skus/create", null, Set.of("skuId", "terms", "stock"), Map.of(), null, true, null),
            new Command("mall", "skus/update", null, Set.of("skuId", "expectedRevision", "terms"), Map.of(), null, true, null),
            new Command("mall", "skus/stock", null, Set.of("skuId", "expectedRevision", "delta"), Map.of(), null, true, null),
            // 账号类(login LoginAdmin,口令):三个方法重放都收敛到同一个终态,可以重试。
            new Command("account", "ScheduleAccountDeletion", "loginpb.LoginAdmin/ScheduleAccountDeletion", Set.of("playerId"),
                    Map.of("operator", OPERATOR, "reason", REASON), null, true, null),
            new Command("account", "RemovePlayersFromAccounts", "loginpb.LoginAdmin/RemovePlayersFromAccounts", Set.of("playerIds"),
                    Map.of("operator", OPERATOR, "reason", REASON, "dryRun", false), null, true, Map.of("dryRun", true)),
            new Command("account", "ResetAccountRealName", "loginpb.LoginAdmin/ResetAccountRealName", Set.of("playerId", "account"),
                    Map.of("operator", OPERATOR, "reason", REASON), null, true, null),
            // 回收(data_service,口令):批次按请求号判重;撤批次、终结回收行按状态收敛。批次先演练拿到 planDigest 再发起。
            new Command("recall", "BatchRecallItems", "data_service.DataService/BatchRecallItems",
                    Set.of("playerIds", "itemConfigId", "currencyType", "timeStart", "timeEnd", "txTypes", "recallKind", "expectedPlanDigest"),
                    Map.of("operator", OPERATOR, "reason", REASON, "dryRun", false), "requestId", true, Map.of("dryRun", true)),
            new Command("recall", "CancelRecallBatch", "data_service.DataService/CancelRecallBatch", Set.of("requestId"),
                    Map.of("operator", OPERATOR, "reason", REASON), null, true, null),
            new Command("recall", "ResolveRecallOp", "data_service.DataService/ResolveRecallOp", Set.of("opId", "finalStatus"),
                    Map.of("operator", OPERATOR, "reason", REASON), null, true, null),
            // 登记「这条指令已人工处置」:之后这些流水永久按已消费处理,以后的回收都跳过,做完撤不回;重放回「早已登记」。
            new Command("recall", "RegisterRecallReviewHandled", "data_service.DataService/RegisterRecallReviewHandled",
                    Set.of("recallKind", "itemConfigId", "currencyType", "playerId", "correlationId", "txIds", "reviewReason"),
                    Map.of("operator", OPERATOR, "reason", REASON), null, true, null),
            // 收件箱处置(player_inbox,口令):处罚按请求号 + 内容摘要判重,冻结时刻随申请冻结;强制作废重放会被当成已是终态,不能重试。
            new Command("inbox", "GmSanctionPlayer", "player_inbox.PlayerInboxAdmin/GmSanctionPlayer",
                    Set.of("playerId", "blockGain", "unblockGain", "attachDebt", "blockItemGain", "unblockItemGain"),
                    Map.of("operator", OPERATOR, "reason", REASON, "issuedAtMs", NOW_MS), "requestId", true, null),
            // VoidItem 的 reason 是作废原因枚举(发起人选),申请原因写进 note(强制模式必填,进审计)。
            new Command("inbox", "VoidItem", "player_inbox.PlayerInboxAdmin/VoidItem", Set.of("playerId", "domain", "seq", "reason", "mode"),
                    Map.of("operator", OPERATOR, "note", REASON), null, false, null),
            // 榜单(rank RankAdmin,调用方 admin):按 (榜, 代, 请求号) 判重。请求里没有操作人与原因字段,两人记在批准记录里。
            // 后台能删任意榜上的单个条目;整代隐藏只许开榜方(榜都是 match 开的),所以目录里不放 DeleteRankGeneration。
            new Command("rank", "RemoveRankEntry", "rankpb.RankAdmin/RemoveRankEntry", Set.of("boardId", "generation", "entityId"),
                    Map.of(), "requestId", true, null),
            // 滞留阵容(match,调用方 ops-formation):终结后重放回「已有终局」,不能重试;发起前先演练拿到成局摘要。
            new Command("match", "ResolveStrandedFormation", "match.MatchInternal/ResolveStrandedFormation", Set.of("battleId", "expectDigest", "expectTarget"),
                    Map.of("operatorName", OPERATOR, "reason", REASON, "mode", "STRANDED_FORMATION_MODE_ABORT"), null, false,
                    Map.of("mode", "STRANDED_FORMATION_MODE_DRY_RUN")));

    private static Map<String, Command> catalog(Command... commands) {
        Map<String, Command> result = new LinkedHashMap<>();
        for (Command command : commands) result.put(command.target() + "/" + command.name(), command);
        return Map.copyOf(result);
    }

    private final ObjectMapper json;
    private final GmCommandJournal journal;
    private final GameMallAdmin mall;
    private final GameOpsRpc rpc;

    public GmOpsController(ObjectMapper json, GmCommandJournal journal, GameMallAdmin mall, GameOpsRpc rpc) {
        this.json = json; this.journal = journal; this.mall = mall; this.rpc = rpc;
    }

    private boolean enabled(String target) { return target.equals("mall") ? mall.enabled() : rpc.enabled(target); }

    @GetMapping("/status")
    public String status() {
        GmAccess.authenticated();
        ObjectNode result = json.createObjectNode().put("dualControl", journal.dualControl());
        ObjectNode targets = result.putObject("targets");
        for (String target : TARGETS) targets.put(target, enabled(target));
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

    /** 预览:演练(dryRun / DRY_RUN)直接调下游,不进流水、不带批准记录;发起前用它拿到计划摘要,批准人用它核对现状。 */
    @PostMapping("/preview")
    public String preview(@RequestBody String body) {
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> { if (!Set.of("target", "command", "body", "reason").contains(name)) bad("存在未知字段"); });
        Command command = command(input);
        if (command.preview() == null) bad("这条命令没有预览");
        String actor = GmAccess.operator(permission(command.target(), "read"));
        String reason = input.has("reason") ? text(input, "reason", 1, 191) : "preview";
        ObjectNode request = frozenBody(command, input, actor, reason);
        command.preview().forEach((field, value) -> request.set(field, json.valueToTree(value)));
        return ok(rpc.preview(command, request));
    }

    /** 发起:命令与正文字段按目录核对,服务端字段由这里填,流水再补请求号;草稿冻结后不可改。 */
    @PostMapping("/drafts")
    public String prepare(@RequestBody String body) {
        ObjectNode input = parse(body);
        input.fieldNames().forEachRemaining(name -> {
            if (!Set.of("clientRequestId", "target", "command", "body", "reason").contains(name)) bad("存在未知字段");
        });
        Command command = command(input);
        String actor = GmAccess.operator(permission(command.target(), "prepare"));
        String clientRequestId = text(input, "clientRequestId", 36, 36);
        try { if (!UUID.fromString(clientRequestId).toString().equals(clientRequestId)) bad("请求号无效"); }
        catch (IllegalArgumentException invalid) { bad("请求号无效"); }
        String reason = text(input, "reason", 1, 191);
        if (reason.codePoints().anyMatch(Character::isISOControl)) bad("原因不能带换行或控制字符");
        // 下游里最严的上限是 191 字节(match 滞留阵容、交易):在发起时就挡住,别等到批准时才被下游拒绝。
        if (reason.getBytes(StandardCharsets.UTF_8).length > 191) bad("原因最多 191 字节(约 63 个汉字)");
        ObjectNode frozen = json.createObjectNode().put("target", command.target()).put("command", command.name());
        frozen.set("body", frozenBody(command, input, actor, reason));
        frozen.put("retryable", command.retryable());
        return ok(journal.prepareOps(actor, clientRequestId, reason, frozen));
    }

    @PostMapping("/commands/{id}/send")
    public String send(@PathVariable String id) {
        Command command = commandOf(id);
        String actor = GmAccess.operator(permission(command.target(), "approve"));
        if (!enabled(command.target())) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "该下游的管理通道未配置");
        BiFunction<ObjectNode, ObjectNode, ObjectNode> sender = command.method() == null ? mall::execute
                : (request, approval) -> rpc.execute(command, request, approval);
        return ok(journal.sendOps(id, actor, sender));
    }

    /** 有批准权限的人可以驳回任何人的待批申请;只有发起权限的人只能撤回自己发起的那一条。 */
    @PostMapping("/commands/{id}/decline")
    public String decline(@PathVariable String id, @RequestBody String body) {
        String target = commandOf(id).target();
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

    private Command command(ObjectNode input) {
        Command command = COMMANDS.get(text(input, "target", 1, 32) + "/" + text(input, "command", 1, 64));
        if (command == null) bad("未知的运营处置命令");
        return command;
    }

    /** 发起人只能填目录里的字段;服务端字段在这里填,发起人带了同名字段就拒绝(目录里两者不重叠)。 */
    private ObjectNode frozenBody(Command command, ObjectNode input, String actor, String reason) {
        if (!(input.get("body") instanceof ObjectNode fields)) { bad("body 必须是对象"); return null; }
        fields.fieldNames().forEachRemaining(name -> { if (!command.fields().contains(name)) bad("命令不接受字段 " + name); });
        ObjectNode body = fields.deepCopy();
        command.filled().forEach((field, value) -> {
            if (REASON.equals(value)) body.put(field, reason);
            else if (OPERATOR.equals(value)) body.put(field, actor);
            else if (NOW_MS.equals(value)) body.put(field, Long.toString(System.currentTimeMillis()));
            else body.set(field, json.valueToTree(value));
        });
        return body;
    }

    /** 命令取自流水里冻结的请求;目录里已经没有的命令一律拒绝。 */
    private Command commandOf(String id) {
        GmAccess.authenticated();
        JsonNode request = journal.get(positive(id), "OPS").path("request");
        Command command = COMMANDS.get(request.path("target").asText("") + "/" + request.path("command").asText(""));
        if (command == null) throw new ResponseStatusException(HttpStatus.CONFLICT, "这条申请的命令已不受支持");
        return command;
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
    public Resp