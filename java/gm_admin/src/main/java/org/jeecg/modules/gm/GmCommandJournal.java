package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.nio.charset.StandardCharsets;
import java.security.SecureRandom;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Clock;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.function.BiFunction;
import java.util.function.Function;
import java.util.function.LongSupplier;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Lazy;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;
import org.springframework.web.server.ResponseStatusException;

/**
 * 管理库中的冻结命令和发送意图；不持有游戏资产，也不在数据库事务内执行网络调用。
 *
 * <p>双人复核（docs/design/admin-identity-and-dual-control.md §5.3）也在这里，而且只在这里：
 * 草稿由发起人创建后不可改；开启双人复核时，第一次放行必须由另一个人来做，放行即执行。
 * 「两人不同」判两遍：Java 里先比较并给出明确报错，状态迁移的 UPDATE 条件里再带一次，两处都是按字节比较。
 * 批准人只记第一次；结果未知后的重试不改批准人，因为这条命令已经过了双人。
 */
@Service
@Lazy(false)
public final class GmCommandJournal {
    private static final Logger LOG = LoggerFactory.getLogger(GmCommandJournal.class);
    static final long PLAYER_DRAFT_TTL_MS = 300_000L;
    private static final int MAX_PAGE = 10_000;
    private static final int MAX_PAGE_SIZE = 100;
    private static final int CAMPAIGN_SAMPLE_LIMIT = 20;
    private static final long MAIL_LEASE_MS = 60_000L;
    private static final long CAMPAIGN_LEASE_MS = 120_000L;
    private static final List<String> FROZEN_MAIL_FIELDS = List.of("requestId", "request_id", "expireMs", "expire_ms",
            "auth", "approval", "recipient_player_id", "campaignId", "campaign_id", "manifestGeneration", "manifest_generation");
    // REJECTED 是游戏服务拒绝执行；DECLINED 是批准人驳回或发起人撤回，命令从未发出。
    private static final Set<String> MAIL_STATUSES = Set.of("DRAFT", "SENDING", "DELIVERED", "REJECTED", "UNKNOWN", "DECLINED");
    private static final Set<String> PLAYER_STATUSES = Set.of("DRAFT", "SENDING", "EXECUTED", "REJECTED", "UNKNOWN", "DECLINED");
    private final JdbcTemplate jdbc;
    private final ObjectMapper mapper;
    private final TransactionTemplate tx;
    private final Clock clock;
    private final LongSupplier ids;
    private final boolean dualControl;
    private final long approvalTtlMs;

    @Autowired
    public GmCommandJournal(JdbcTemplate jdbc, ObjectMapper mapper, PlatformTransactionManager manager,
            @Value("${gm.identity.mode:native}") String identityMode, @Value("${gm.dual-control.enforce:}") String enforce,
            @Value("${gm.dual-control.approval-ttl-hours:24}") long approvalTtlHours) {
        this(jdbc, mapper, manager, Clock.systemUTC(), new SecureRandom()::nextLong,
                dualControlEnforced(identityMode, enforce), approvalTtlHours);
    }

    /** 不开双人复核的流水：行为与引入双人复核之前相同，只有草稿创建者可以发送。 */
    GmCommandJournal(JdbcTemplate jdbc, ObjectMapper mapper, PlatformTransactionManager manager,
            Clock clock, LongSupplier ids) {
        this(jdbc, mapper, manager, clock, ids, false, 24);
    }

    GmCommandJournal(JdbcTemplate jdbc, ObjectMapper mapper, PlatformTransactionManager manager,
            Clock clock, LongSupplier ids, boolean dualControl, long approvalTtlHours) {
        if (approvalTtlHours < 1 || approvalTtlHours > 168)
            throw new IllegalStateException("gm.dual-control.approval-ttl-hours must be between 1 and 168");
        this.jdbc = jdbc;
        this.mapper = mapper;
        this.clock = clock;
        this.ids = ids;
        this.dualControl = dualControl;
        this.approvalTtlMs = approvalTtlHours * 3_600_000L;
        tx = new TransactionTemplate(manager);
        tx.setPropagationBehavior(TransactionDefinition.PROPAGATION_REQUIRES_NEW);
    }

    /**
     * 双人复核开不开。OIDC 模式（生产）恒开，显式关掉则拒绝启动；原生口令模式（本机开发）缺省关，
     * 一个人能跑通，也可以显式打开用两个本地账号测。
     */
    static boolean dualControlEnforced(String identityMode, String configured) {
        boolean oidc = GmOidcLogin.isOidc(identityMode);
        if (configured == null || configured.isBlank()) return oidc;
        if (!configured.equals("true") && !configured.equals("false"))
            throw new IllegalStateException("gm.dual-control.enforce must be true or false");
        boolean enforce = configured.equals("true");
        if (oidc && !enforce) throw new IllegalStateException("gm.dual-control.enforce cannot be false in OIDC mode");
        return enforce;
    }

    public boolean dualControl() { return dualControl; }

    public ObjectNode prepare(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, playerId, reason, input, "MAIL");
    }

    public ObjectNode prepareSystemMail(String operator, String clientRequestId, String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, "0", reason, input, "SYSTEM_MAIL");
    }

    public ObjectNode prepareMailCampaign(String operator, String clientRequestId, String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, "0", reason, input, "MAIL_CAMPAIGN");
    }

    public ObjectNode preparePlayerCommand(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, playerId, reason, input, "PLAYER");
    }

    /** 交易人工处置(docs/design/admin-identity-and-dual-control.md §5.6):冻结 ResolveManualContract 的业务字段,不绑定单个玩家。 */
    public ObjectNode prepareTradeManual(String operator, String clientRequestId, String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, "0", reason, input, "TRADE_MANUAL");
    }

    /**
     * 运营处置(docs/design/admin-identity-and-dual-control.md §8 批 4、批 5):商城人工单与 SKU、账号类、回收、收件箱、榜单、阵容。
     * input 是 {target, command, body, retryable}:下游与命令由 GmOpsController 的目录核过,操作人、原因等服务端字段已由它填进 body;
     * retryable 也由目录给出并随申请冻结:下游按批准号判重或者重放天然收敛的命令,结果未知后可以用原申请重试;
     * 重放会得到「已是终态」之类拒绝的命令(强制作废收件箱条目、终结滞留阵容)不能重试,未知结果要人去下游核对。
     * 流水再补 requestId(就是流水号,也就是批准号)。
     */
    public ObjectNode prepareOps(String operator, String clientRequestId, String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, "0", reason, input, "OPS");
    }

    private ObjectNode prepareInternal(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input, String kind) {
        check(operator != null && !operator.isBlank() && bytes(operator).length <= 512, "操作者非法");
        check(clientRequestId != null && clientRequestId.matches("[A-Za-z0-9._:-]{1,128}"), "客户端幂等号非法");
        if (kind.equals("SYSTEM_MAIL") || kind.equals("MAIL_CAMPAIGN") || kind.equals("TRADE_MANUAL") || kind.equals("OPS")) {
            check("0".equals(playerId), "该类命令不绑定单个玩家");
        } else {
            check(playerId != null && playerId.matches("[1-9][0-9]{0,19}"), "玩家 ID 非法");
        }
        check(reason != null && !reason.isBlank() && reason.length() <= 1000, "操作原因非法");
        check(input != null, "命令内容不能为空");

        int days = 0;
        if (kind.equals("MAIL_CAMPAIGN")) {
            check(input.size() == 2 && input.get("mail") instanceof ObjectNode && input.get("shards") instanceof ArrayNode, "活动邮件草稿格式非法");
            ObjectNode mailInput = (ObjectNode) input.get("mail");
            ArrayNode shardsInput = (ArrayNode) input.get("shards");
            check(!shardsInput.isEmpty(), "活动名单分片不能为空");
            for (JsonNode shard : shardsInput) check(shard.isArray() && !shard.isEmpty(), "活动名单分片格式非法");
            days = validDays(mailInput);
            rejectFrozenFields(mailInput, true);
        } else if (kind.equals("MAIL") || kind.equals("SYSTEM_MAIL")) {
            days = validDays(input);
            rejectFrozenFields(input, kind.equals("SYSTEM_MAIL"));
            if (kind.equals("MAIL")) {
                check(!input.has("recipientPlayerId") || playerId.equals(input.path("recipientPlayerId").asText()), "玩家不一致");
            }
        } else if (kind.equals("OPS")) {
            check(input.size() == 4 && input.path("target").isTextual() && input.path("command").isTextual() && input.get("body") instanceof ObjectNode
                    && input.path("retryable").isBoolean(), "运营处置格式非法");
            // 批准记录由服务端在放行时补,调用证明在发送时现签,草稿里不许出现。请求号在顶层冻结,由发送方按目录填进下游的请求号字段
            // (正文里同名的 requestId 可能是别的含义,如撤回回收批次时的批次号,由目录的字段白名单管)。
            for (String frozen : List.of("approval", "auth"))
                check(!input.path("body").has(frozen), "不得覆盖服务端冻结字段");
        } else if (kind.equals("TRADE_MANUAL")) {
            check(input.path("commerceId").isTextual() && input.path("action").isTextual() && input.path("expectVersion").isTextual()
                    && input.path("expectTarget").isTextual(), "交易人工处置格式非法");
            // 操作人、原因由服务端冻结;模式与批准记录在放行时由服务端补上,草稿里不许出现。
            for (String frozen : List.of("operator", "reason", "mode", "approval", "auth", "requestId"))
                check(!input.has(frozen), "不得覆盖服务端冻结字段");
        } else {
            check(input.path("operation").isTextual() && input.path("arguments").isObject(), "玩家命令格式非法");
            check(!input.has("operator") && !input.has("reason") && !input.has("issuedAtMs") && !input.has("playerId"), "不得覆盖服务端冻结字段");
            rejectFrozenFields(input, true);
        }

        ObjectNode original = input.deepCopy();
        List<Row> previous = byKey(operator, clientRequestId);
        if (!previous.isEmpty()) return replay(previous.get(0), playerId, reason, original, kind);
        for (int attempt = 0; attempt < 3; attempt++) {
            String id = nextId();
            long now = clock.millis();
            ObjectNode request = original.deepCopy();
            if (kind.equals("MAIL")) {
                request.remove("validDays");
                request.put("requestId", id)
                        .put("recipientPlayerId", playerId)
                        .put("expireMs", Long.toString(Math.addExact(now, days * 86_400_000L)));
            } else if (kind.equals("SYSTEM_MAIL")) {
                request.remove("validDays");
                request.put("requestId", id)
                        .put("expireMs", Long.toString(Math.addExact(now, days * 86_400_000L)));
            } else if (kind.equals("MAIL_CAMPAIGN")) {
                ObjectNode mailRequest = (ObjectNode) request.get("mail");
                mailRequest.remove("validDays");
                mailRequest.put("requestId", id)
                        .put("campaignId", id)
                        .put("manifestGeneration", "1")
                        .put("expireMs", Long.toString(Math.addExact(now, days * 86_400_000L)));
            } else if (kind.equals("TRADE_MANUAL")) {
                request.put("operator", operator).put("reason", reason);
            } else if (kind.equals("OPS")) {
                request.put("requestId", id);
            } else {
                request.put("requestId", id)
                        .put("playerId", playerId).put("operator", operator).put("reason", reason)
                        .put("issuedAtMs", Long.toString(now));
            }
            try {
                return tx.execute(status -> {
                    jdbc.update("INSERT INTO gm_command (id,command_type,operator_name,client_request_id,player_id,reason,input_json,"
                            + "request_json,status,attempt,created_ms,updated_ms) VALUES (?,?,?,?,?,?,?,?,'DRAFT',0,?,?)",
                            id, kind, bytes(operator), bytes(clientRequestId), playerId, reason, original.toString(), request.toString(), now, now);
                    audit(id, operator, "DRAFT", 0, null, now);
                    return get(id);
                });
            } catch (DuplicateKeyException collision) {
                previous = byKey(operator, clientRequestId);
                if (!previous.isEmpty()) return replay(previous.get(0), playerId, reason, original, kind);
            }
        }
        throw conflict("命令号冲突，请重新创建草稿");
    }

    private static int validDays(ObjectNode node) {
        check(node.path("validDays").isIntegralNumber() && node.path("validDays").canConvertToInt(), "有效天数非法");
        int days = node.path("validDays").asInt();
        check(days >= 1 && days <= 30, "有效天数须为 1 至 30");
        return days;
    }

    private static void rejectFrozenFields(ObjectNode node, boolean forbidRecipientPlayerId) {
        for (String field : FROZEN_MAIL_FIELDS) {
            check(!node.has(field), "不得覆盖服务端冻结字段");
        }
        if (forbidRecipientPlayerId) {
            check(!node.has("recipientPlayerId"), "不得覆盖服务端冻结字段");
        }
    }

    public ObjectNode get(String id) { return row(id, false).view; }

    public ObjectNode get(String id, String kind) {
        check(id != null && id.matches("[1-9][0-9]{0,19}"), "命令 ID 非法");
        Row command = row(id, false);
        if (!kind.equals(command.view.path("commandType").asText())) throw conflict("命令类型与操作入口不符");
        return command.view;
    }

    public ObjectNode list() {
        return list(1, 50, null, null);
    }

    public ObjectNode list(int page, int pageSize, String playerId, String status) {
        return list("MAIL", page, pageSize, playerId, status);
    }

    public ObjectNode listSystemMails(int page, int pageSize, String status) {
        return list("SYSTEM_MAIL", page, pageSize, null, status);
    }

    public ObjectNode listMailCampaigns(int page, int pageSize, String status) {
        return list("MAIL_CAMPAIGN", page, pageSize, null, status);
    }

    public ObjectNode listTradeManual(int page, int pageSize, String status) {
        return list("TRADE_MANUAL", page, pageSize, null, status);
    }

    public ObjectNode listOps(int page, int pageSize, String status) {
        return list("OPS", page, pageSize, null, status);
    }

    public ObjectNode listPlayerCommands() {
        return listPlayerCommands(1, 50, null, null);
    }

    public ObjectNode listPlayerCommands(int page, int pageSize, String playerId, String status) {
        return list("PLAYER", page, pageSize, playerId, status);
    }

    private ObjectNode list(String kind, int page, int pageSize, String playerId, String status) {
        check(page >= 1 && page <= MAX_PAGE, "页码须为 1 至 " + MAX_PAGE);
        check(pageSize >= 1 && pageSize <= MAX_PAGE_SIZE, "每页条数须为 1 至 " + MAX_PAGE_SIZE);
        StringBuilder where = new StringBuilder("WHERE command_type=?");
        List<Object> filterArgs = new ArrayList<>();
        filterArgs.add(kind);
        if (playerId != null && !playerId.isBlank()) {
            String trimmed = playerId.strip();
            check(trimmed.matches("[1-9][0-9]{0,19}"), "筛选玩家 ID 非法");
            where.append(" AND player_id=?");
            filterArgs.add(trimmed);
        }
        if (status != null && !status.isBlank()) {
            String trimmed = status.strip();
            Set<String> allowed = isMailKind(kind) ? MAIL_STATUSES : PLAYER_STATUSES;
            check(allowed.contains(trimmed), "筛选状态非法");
            where.append(" AND status=?");
            filterArgs.add(trimmed);
        }
        List<Object> pageArgs = new ArrayList<>(filterArgs);
        pageArgs.add(pageSize);
        pageArgs.add((long) (page - 1) * pageSize);
        ObjectNode result = mapper.createObjectNode();
        var records = result.putArray("records");
        // 列表只读取视图字段，避免将活动邮件的原始 input_json 大名单一并拉入内存。
        jdbc.query("SELECT id,command_type,operator_name,approver_name,approved_ms,player_id,reason,request_json,status,result_json,created_ms,updated_ms FROM gm_command "
                + where + " ORDER BY created_ms DESC,id DESC LIMIT ? OFFSET ?",
                this::mapView, pageArgs.toArray()).forEach(records::add);
        Long total = jdbc.queryForObject("SELECT COUNT(*) FROM gm_command " + where, Long.class, filterArgs.toArray());
        result.put("total", total == null ? 0L : total);
        result.put("page", page);
        result.put("pageSize", pageSize);
        return result;
    }

    public ObjectNode auditHistory(String id, String kind) {
        check(id != null && id.matches("[1-9][0-9]{0,19}"), "命令 ID 非法");
        Row command = row(id, false);
        if (!kind.equals(command.view.path("commandType").asText())) throw conflict("命令类型与操作入口不符");
        ObjectNode result = mapper.createObjectNode().put("commandId", id);
        var records = result.putArray("records");
        jdbc.query("SELECT id,operator_name,event,attempt,result_json,created_ms FROM gm_command_audit WHERE command_id=? ORDER BY id ASC",
                (rs, index) -> {
                    ObjectNode entry = mapper.createObjectNode()
                            .put("id", Long.toString(rs.getLong("id")))
                            .put("operator", new String(rs.getBytes("operator_name"), StandardCharsets.UTF_8))
                            .put("event", rs.getString("event"))
                            .put("attempt", rs.getLong("attempt"))
                            .put("createdAt", Instant.ofEpochMilli(rs.getLong("created_ms")).toString());
                    String rawResult = rs.getString("result_json");
                    entry.set("result", rawResult == null ? mapper.nullNode() : parse(rawResult));
                    return entry;
                }, id).forEach(records::add);
        return result;
    }

    /**
     * 驳回一条还没放行的申请；发起人用它撤回自己的申请。驳回后这条命令永远不会发出，要做同一件事得重新发起。
     * ownOnly 为 true 表示操作人只有发起权限、没有放行权限：他只能撤回自己发起的那一条。
     */
    public ObjectNode decline(String id, String operator, String reason, String kind, boolean ownOnly) {
        check(id != null && id.matches("[1-9][0-9]{0,19}"), "命令 ID 非法");
        check(reason != null && !reason.isBlank() && reason.codePointCount(0, reason.length()) <= 191, "驳回原因须为 1 至 191 个字符");
        return tx.execute(status -> {
            Row current = row(id, true);
            if (!kind.equals(current.view.path("commandType").asText())) throw conflict("命令类型与操作入口不符");
            if (ownOnly && !current.view.path("operator").asText().equals(operator)) {
                throw new ResponseStatusException(HttpStatus.FORBIDDEN, "只能撤回自己发起的申请");
            }
            long now = clock.millis();
            ObjectNode outcome = mapper.createObjectNode().put("declinedBy", operator).put("reason", reason);
            int changed = jdbc.update("UPDATE gm_command SET status='DECLINED',result_json=?,updated_ms=? WHERE id=? AND status='DRAFT'",
                    outcome.toString(), now, id);
            if (changed != 1) throw conflict("只有尚未放行的申请可以驳回");
            audit(id, operator, "DECLINED", current.attempt, outcome, now);
            return get(id);
        });
    }

    /**
     * 邮件三类的放行。开了双人复核时,批准记录放进交给发送方的冻结请求(SendMailRequest.approval;活动邮件放在建活动的那份请求里),
     * mail 对调用方 admin 要求它(docs/design/admin-identity-and-dual-control.md §8 批 5)。它进请求摘要,重试原样复用,摘要不变。
     */
    public ObjectNode send(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, (request, approval) -> sender.apply(withApproval(request, approval)), "MAIL");
    }

    public ObjectNode sendSystemMail(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, (request, approval) -> sender.apply(withApproval(request, approval)), "SYSTEM_MAIL");
    }

    public ObjectNode sendMailCampaign(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, (request, approval) -> {
            if (approval != null && request.get("mail") instanceof ObjectNode mail) mail.set("approval", approval);
            return sender.apply(request);
        }, "MAIL_CAMPAIGN");
    }

    private static ObjectNode withApproval(ObjectNode request, ObjectNode approval) {
        if (approval != null) request.set("approval", approval);
        return request;
    }

    public ObjectNode sendPlayerCommand(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, (request, approval) -> sender.apply(request), "PLAYER");
    }

    /**
     * 交易人工处置的放行。sender 的第二个参数是下传给 trade 的批准记录(approvalId、proposer、approver、approvedAtMs);
     * 没开双人复核(本机开发)时为 null,trade 要显式放开才接受。批准记录只在第一次放行时生成,重试原样复用,请求摘要不变。
     */
    public ObjectNode sendTradeManual(String id, String operator, BiFunction<ObjectNode, ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, sender, "TRADE_MANUAL");
    }

    /** 运营处置的放行;sender 的第二个参数是批准记录,与交易人工处置同一口径。 */
    public ObjectNode sendOps(String id, String operator, BiFunction<ObjectNode, ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, sender, "OPS");
    }

    private ObjectNode sendInternal(String id, String operator, BiFunction<ObjectNode, ObjectNode, ObjectNode> sender, String kind) {
        Objects.requireNonNull(sender);
        if (TransactionSynchronizationManager.isActualTransactionActive()) {
            throw new IllegalStateException("Sending inside an ambient transaction is forbidden");
        }
        boolean mailKind = isMailKind(kind);
        // 下游按请求号或批准号判重的命令,结果未知后可以用原号重试;在线玩家的直接 GM 没有这样的判重,不能重试。
        // 运营处置逐条看申请上冻结的 retryable。
        boolean kindRetryable = mailKind || kind.equals("TRADE_MANUAL");
        String complete = mailKind ? "DELIVERED" : "EXECUTED";
        long leaseMs = kind.equals("MAIL_CAMPAIGN") ? CAMPAIGN_LEASE_MS : MAIL_LEASE_MS;
        Row claimed = tx.execute(status -> {
            Row current = row(id, true);
            if (!kind.equals(current.view.path("commandType").asText())) throw conflict("命令类型与操作入口不符");
            String proposer = current.view.path("operator").asText();
            // 还没有批准人的命令，第一次放行就是批准：开了双人复核必须是另一个人，没开则只有创建者本人。
            // 已有批准人的命令只会是结果未知后的重试，谁来重试都不改批准人。
            boolean firstRelease = current.approver == null;
            if (dualControl && firstRelease && proposer.equals(operator)) {
                throw new ResponseStatusException(HttpStatus.FORBIDDEN, "发起人不能批准自己的申请，需要另一名管理员批准");
            }
            if (!dualControl && !proposer.equals(operator)) {
                throw new ResponseStatusException(HttpStatus.FORBIDDEN, "仅草稿创建者可发送");
            }
            if (current.view.path("status").asText().equals(complete)) return current;
            long now = clock.millis();
            if (dualControl && firstRelease && now - current.createdMs > approvalTtlMs) {
                throw conflict("申请已超过批准时限，请重新发起");
            }
            if (mailKind && current.view.path("status").asText().equals("DRAFT")) {
                JsonNode mailNode = kind.equals("MAIL_CAMPAIGN") ? current.fullRequest.path("mail") : current.fullRequest;
                long expireMs = parsePositiveLong(mailNode.path("expireMs").asText(""));
                if (expireMs <= now) {
                    throw conflict("邮件草稿已过设定到期时间，请重新创建草稿");
                }
            }
            if (kind.equals("PLAYER") && current.view.path("status").asText().equals("DRAFT")) {
                long issuedAtMs = parsePositiveLong(current.fullRequest.path("issuedAtMs").asText(""));
                long age = now - issuedAtMs;
                if (issuedAtMs <= 0 || age < 0 || age > PLAYER_DRAFT_TTL_MS) {
                    throw conflict("草稿已过期，请重新创建并核对");
                }
            }
            // 批准人与批准时刻只在第一次放行时写入（COALESCE 保住已有值）；「两人不同」在这条 UPDATE 的条件里再判一次，
            // 即使上面的 Java 判断将来被改坏，库里也写不进「自己批自己」。用户名列是二进制类型，比较不受库的大小写规则影响。
            List<Object> args = new ArrayList<>(List.of(now));
            String approval = "";
            if (dualControl) {
                approval = ",approver_name=COALESCE(approver_name,?),approved_ms=COALESCE(approved_ms,?)";
                args.add(bytes(operator));
                args.add(now);
            }
            args.add(id);
            args.add(current.attempt);
            boolean retryable = kind.equals("OPS") ? current.fullRequest.path("retryable").asBoolean(false) : kindRetryable;
            String states;
            if (retryable) {
                states = "(status IN ('DRAFT','UNKNOWN') OR (status='SENDING' AND updated_ms<?))";
                args.add(now - leaseMs);
            } else {
                // 在线玩家的直接 GM 没有跨进程持久去重；一旦开始执行，未知结果永远不能重发。
                states = "status='DRAFT'";
            }
            String distinct = "";
            if (dualControl) {
                distinct = " AND (approver_name IS NOT NULL OR operator_name<>?)";
                args.add(bytes(operator));
            }
            int changed = jdbc.update("UPDATE gm_command SET status='SENDING',attempt=attempt+1,updated_ms=?" + approval
                    + " WHERE id=? AND attempt=? AND " + states + distinct, args.toArray());
            if (changed != 1) throw conflict("命令正在发送或已拒绝，请核对当前状态");
            if (dualControl && firstRelease) audit(id, operator, "APPROVED", current.attempt + 1, null, now);
            audit(id, operator, "SENDING", current.attempt + 1, null, now);
            return row(id, false);
        });
        if (claimed.view.path("status").asText().equals(complete)) return claimed.view;
        ObjectNode outcome;
        String state;
        try {
            outcome = Objects.requireNonNull(sender.apply(claimed.fullRequest.deepCopy(), approvalOf(claimed)));
            state = complete;
        } catch (GameServices.CallFailure error) {
            outcome = mapper.createObjectNode().put("code", error.getCode()).put("tipId", error.getTipId())
                    .put("outcomeUnknown", error.isOutcomeUnknown());
            state = error.isOutcomeUnknown() ? "UNKNOWN" : "REJECTED";
        } catch (RuntimeException error) {
            outcome = mapper.createObjectNode().put("code", "UNEXPECTED_FAILURE").put("outcomeUnknown", true);
            state = "UNKNOWN";
        }
        final ObjectNode frozenOutcome = outcome.deepCopy();
        final String finalState = state;
        // 结果和审计同事务；落盘失败则保留 SENDING，禁止把未知结果报告成已成功。
        return tx.execute(status -> {
            long now = clock.millis();
            int changed = jdbc.update("UPDATE gm_command SET status=?,result_json=?,updated_ms=? "
                    + "WHERE id=? AND status='SENDING' AND attempt=?", finalState, frozenOutcome.toString(), now, id, claimed.attempt);
            if (changed == 1) audit(id, operator, finalState, claimed.attempt, frozenOutcome, now);
            return get(id);
        });
    }

    private static boolean isMailKind(String kind) {
        return kind.equals("MAIL") || kind.equals("SYSTEM_MAIL") || kind.equals("MAIL_CAMPAIGN");
    }

    private String nextId() {
        for (int i = 0; i < 8; i++) {
            long candidate = ids.getAsLong() & Long.MAX_VALUE;
            if (candidate != 0) return Long.toString(candidate);
        }
        throw new IllegalStateException("Command ID generation failed");
    }

    private static long parsePositiveLong(String value) {
        if (value == null || !value.matches("[1-9][0-9]{0,18}")) return 0L;
        try { return Long.parseLong(value); }
        catch (NumberFormatException error) { return 0L; }
    }

    private ObjectNode replay(Row old, String player, String reason, ObjectNode original, String kind) {
        if (!old.view.path("commandType").asText().equals(kind) || !old.view.path("playerId").asText().equals(player) || !old.view.path("reason").asText().equals(reason)
                || !old.original.equals(original)) throw conflict("同一幂等号不能更换玩家、原因或发放内容");
        return old.view;
    }

    private List<Row> byKey(String operator, String key) {
        return jdbc.query("SELECT * FROM gm_command WHERE operator_name=? AND client_request_id=?",
                this::map, bytes(operator), bytes(key));
    }

    private Row row(String id, boolean lock) {
        List<Row> rows = jdbc.query("SELECT * FROM gm_command WHERE id=?" + (lock ? " FOR UPDATE" : ""), this::map, id);
        if (rows.isEmpty()) throw new ResponseStatusException(HttpStatus.NOT_FOUND, "命令不存在");
        return rows.get(0);
    }

    private ObjectNode mapView(ResultSet rs, int index) throws SQLException {
        String commandType = rs.getString("command_type");
        return buildView(rs, commandType, parse(rs.getString("request_json")));
    }

    private Row map(ResultSet rs, int index) throws SQLException {
        String commandType = rs.getString("command_type");
        ObjectNode fullRequest = parse(rs.getString("request_json"));
        byte[] approver = rs.getBytes("approver_name");
        return new Row(buildView(rs, commandType, fullRequest), fullRequest, parse(rs.getString("input_json")), rs.getLong("attempt"),
                approver == null ? null : new String(approver, StandardCharsets.UTF_8), rs.getLong("created_ms"), rs.getLong("approved_ms"));
    }

    /** 下传给资源服务的批准记录;还没有批准人(没开双人复核)时为 null。流水号同时是资源服务的幂等键。 */
    private ObjectNode approvalOf(Row claimed) {
        if (claimed.approver == null) return null;
        return mapper.createObjectNode().put("approvalId", claimed.view.path("id").asText())
                .put("proposer", claimed.view.path("operator").asText()).put("approver", claimed.approver)
                .put("approvedAtMs", Long.toString(claimed.approvedMs));
    }

    private ObjectNode buildView(ResultSet rs, String commandType, ObjectNode fullRequest) throws SQLException {
        ObjectNode view = mapper.createObjectNode().put("id", rs.getString("id")).put("commandType", commandType).put("status", rs.getString("status"))
                .put("operator", new String(rs.getBytes("operator_name"), StandardCharsets.UTF_8))
                .put("playerId", rs.getString("player_id")).put("reason", rs.getString("reason"))
                .put("createdAt", Instant.ofEpochMilli(rs.getLong("created_ms")).toString())
                .put("updatedAt", Instant.ofEpochMilli(rs.getLong("updated_ms")).toString());
        // 批准人与批准时刻在第一次放行前为空；页面据此区分「待批准」与「已批准」。
        byte[] approver = rs.getBytes("approver_name");
        long approvedMs = rs.getLong("approved_ms");
        view.put("approver", approver == null ? null : new String(approver, StandardCharsets.UTF_8));
        view.put("approvedAt", approver == null || approvedMs == 0 ? null : Instant.ofEpochMilli(approvedMs).toString());
        view.set("request", commandType.equals("MAIL_CAMPAIGN") ? compactCampaignRequest(fullRequest) : fullRequest.deepCopy());
        if (commandType.equals("PLAYER")) view.put("operation", fullRequest.path("operation").asText());
        String result = rs.getString("result_json");
        view.set("result", result == null ? mapper.nullNode() : parse(result));
        return view;
    }

    private ObjectNode compactCampaignRequest(ObjectNode fullRequest) {
        ObjectNode compact = mapper.createObjectNode();
        JsonNode mail = fullRequest.get("mail");
        if (mail != null) compact.set("mail", mail.deepCopy());
        ArrayNode sample = compact.putArray("recipientSample");
        long total = 0;
        int shardCount = 0;
        JsonNode shards = fullRequest.get("shards");
        if (shards != null && shards.isArray()) {
            shardCount = shards.size();
            for (JsonNode shard : shards) {
                if (!shard.isArray()) continue;
                total += shard.size();
                for (JsonNode id : shard) {
                    if (sample.size() < CAMPAIGN_SAMPLE_LIMIT) sample.add(id.asText());
                    else break;
                }
            }
        }
        compact.put("totalRecipients", total);
        compact.put("shardCount", shardCount);
        return compact;
    }

    private ObjectNode parse(String json) {
        try { return (ObjectNode) mapper.readTree(json); }
        catch (JsonProcessingException error) { throw new IllegalStateException("Persisted command JSON is invalid", error); }
    }

    private void audit(String id, String operator, String event, long attempt, ObjectNode result, long now) {
        jdbc.update("INSERT INTO gm_command_audit (command_id,operator_name,event,attempt,result_json,created_ms) VALUES (?,?,?,?,?,?)",
                id, bytes(operator), event, attempt, result == null ? null : result.toString(), now);
        // 审计行提交之后再打一行日志进日志系统：库里的行以后若被改动，可以和日志对出来。回滚的事务不打。
        Runnable line = () -> LOG.info("GM command audit command_id={} event={} operator={} attempt={}", id, event, operator, attempt);
        if (TransactionSynchronizationManager.isSynchronizationActive()) {
            TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
                @Override public void afterCommit() { line.run(); }
            });
        } else {
            line.run();
        }
    }

    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }
    private static void check(boolean allowed, String message) {
        if (!allowed) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, message);
    }
    private static ResponseStatusException conflict(String message) { return new ResponseStatusException(HttpStatus.CONFLICT, message); }
    /** approver 为 null 表示还没有人放行过；createdMs 用来判申请是否过了批准时限。 */
    private record Row(ObjectNode view, ObjectNode fullRequest, ObjectNode original, long attempt, String approver, long createdMs, long approvedMs) { }
}
