package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
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
import java.util.function.Function;
import java.util.function.LongSupplier;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Lazy;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;
import org.springframework.web.server.ResponseStatusException;

/** 管理库中的冻结命令和发送意图；不持有游戏资产，也不在数据库事务内执行网络调用。 */
@Service
@Lazy(false)
public final class GmCommandJournal {
    static final long PLAYER_DRAFT_TTL_MS = 300_000L;
    private static final int MAX_PAGE = 10_000;
    private static final int MAX_PAGE_SIZE = 100;
    private static final Set<String> MAIL_STATUSES = Set.of("DRAFT", "SENDING", "DELIVERED", "REJECTED", "UNKNOWN");
    private static final Set<String> PLAYER_STATUSES = Set.of("DRAFT", "SENDING", "EXECUTED", "REJECTED", "UNKNOWN");
    private final JdbcTemplate jdbc;
    private final ObjectMapper mapper;
    private final TransactionTemplate tx;
    private final Clock clock;
    private final LongSupplier ids;

    @Autowired
    public GmCommandJournal(JdbcTemplate jdbc, ObjectMapper mapper, PlatformTransactionManager manager) {
        this(jdbc, mapper, manager, Clock.systemUTC(), new SecureRandom()::nextLong);
    }

    GmCommandJournal(JdbcTemplate jdbc, ObjectMapper mapper, PlatformTransactionManager manager,
            Clock clock, LongSupplier ids) {
        this.jdbc = jdbc;
        this.mapper = mapper;
        this.clock = clock;
        this.ids = ids;
        tx = new TransactionTemplate(manager);
        tx.setPropagationBehavior(TransactionDefinition.PROPAGATION_REQUIRES_NEW);
    }

    public ObjectNode prepare(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, playerId, reason, input, "MAIL");
    }

    public ObjectNode preparePlayerCommand(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input) {
        return prepareInternal(operator, clientRequestId, playerId, reason, input, "PLAYER");
    }

    private ObjectNode prepareInternal(String operator, String clientRequestId, String playerId,
            String reason, ObjectNode input, String kind) {
        check(operator != null && !operator.isBlank() && bytes(operator).length <= 512, "操作者非法");
        check(clientRequestId != null && clientRequestId.matches("[A-Za-z0-9._:-]{1,128}"), "客户端幂等号非法");
        check(playerId != null && playerId.matches("[1-9][0-9]{0,19}"), "玩家 ID 非法");
        check(reason != null && !reason.isBlank() && reason.length() <= 1000, "操作原因非法");
        check(input != null, "命令内容不能为空");
        int days = input.path("validDays").asInt();
        if (kind.equals("MAIL")) {
            check(input.path("validDays").isIntegralNumber() && input.path("validDays").canConvertToInt(), "有效天数非法");
            check(days >= 1 && days <= 30, "有效天数须为 1 至 30");
        } else {
            check(input.path("operation").isTextual() && input.path("arguments").isObject(), "玩家命令格式非法");
            check(!input.has("operator") && !input.has("reason") && !input.has("issuedAtMs") && !input.has("playerId"), "不得覆盖服务端冻结字段");
        }
        for (String field : List.of("requestId", "request_id", "expireMs", "expire_ms", "auth", "recipient_player_id")) {
            check(!input.has(field), "不得覆盖服务端冻结字段");
        }
        check(!input.has("recipientPlayerId") || playerId.equals(input.path("recipientPlayerId").asText()), "玩家不一致");
        ObjectNode original = input.deepCopy();
        List<Row> previous = byKey(operator, clientRequestId);
        if (!previous.isEmpty()) return replay(previous.get(0), playerId, reason, original, kind);
        for (int attempt = 0; attempt < 3; attempt++) {
            String id = nextId();
            long now = clock.millis();
            ObjectNode request = original.deepCopy();
            request.put("requestId", id);
            if (kind.equals("MAIL")) {
                request.remove("validDays");
                request.put("recipientPlayerId", playerId).put("expireMs", Long.toString(Math.addExact(now, days * 86_400_000L)));
            } else {
                request.put("playerId", playerId).put("operator", operator).put("reason", reason).put("issuedAtMs", Long.toString(now));
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
            Set<String> allowed = kind.equals("MAIL") ? MAIL_STATUSES : PLAYER_STATUSES;
            check(allowed.contains(trimmed), "筛选状态非法");
            where.append(" AND status=?");
            filterArgs.add(trimmed);
        }
        List<Object> pageArgs = new ArrayList<>(filterArgs);
        pageArgs.add(pageSize);
        pageArgs.add((long) (page - 1) * pageSize);
        ObjectNode result = mapper.createObjectNode();
        var records = result.putArray("records");
        jdbc.query("SELECT * FROM gm_command " + where + " ORDER BY created_ms DESC,id DESC LIMIT ? OFFSET ?",
                this::map, pageArgs.toArray()).forEach(row -> records.add(row.view));
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

    public ObjectNode send(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, sender, "MAIL");
    }

    public ObjectNode sendPlayerCommand(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        return sendInternal(id, operator, sender, "PLAYER");
    }

    private ObjectNode sendInternal(String id, String operator, Function<ObjectNode, ObjectNode> sender, String kind) {
        Objects.requireNonNull(sender);
        if (TransactionSynchronizationManager.isActualTransactionActive()) {
            throw new IllegalStateException("Sending inside an ambient transaction is forbidden");
        }
        Row claimed = tx.execute(status -> {
            Row current = row(id, true);
            if (!kind.equals(current.view.path("commandType").asText())) throw conflict("命令类型与操作入口不符");
            if (!current.view.path("operator").asText().equals(operator)) {
                throw new ResponseStatusException(HttpStatus.FORBIDDEN, "仅草稿创建者可发送");
            }
            String complete = kind.equals("MAIL") ? "DELIVERED" : "EXECUTED";
            if (current.view.path("status").asText().equals(complete)) return current;
            long now = clock.millis();
            if (kind.equals("MAIL") && current.view.path("status").asText().equals("DRAFT")) {
                long expireMs = parsePositiveLong(current.view.path("request").path("expireMs").asText(""));
                if (expireMs <= now) {
                    throw conflict("邮件草稿已过设定到期时间，请重新创建草稿");
                }
            }
            if (kind.equals("PLAYER") && current.view.path("status").asText().equals("DRAFT")) {
                long issuedAtMs = parsePositiveLong(current.view.path("request").path("issuedAtMs").asText(""));
                long age = now - issuedAtMs;
                if (issuedAtMs <= 0 || age < 0 || age > PLAYER_DRAFT_TTL_MS) {
                    throw conflict("草稿已过期，请重新创建并核对");
                }
            }
            int changed;
            if (kind.equals("MAIL")) {
                changed = jdbc.update("UPDATE gm_command SET status='SENDING',attempt=attempt+1,updated_ms=? "
                        + "WHERE id=? AND attempt=? AND (status IN ('DRAFT','UNKNOWN') OR (status='SENDING' AND updated_ms<?))",
                        now, id, current.attempt, now - 60_000);
            } else {
                // 在线玩家的直接 GM 没有跨进程持久去重；一旦开始执行，未知结果永远不能重发。
                changed = jdbc.update("UPDATE gm_command SET status='SENDING',attempt=attempt+1,updated_ms=? "
                        + "WHERE id=? AND attempt=? AND status='DRAFT'", now, id, current.attempt);
            }
            if (changed != 1) throw conflict("命令正在发送或已拒绝，请核对当前状态");
            audit(id, operator, "SENDING", current.attempt + 1, null, now);
            return row(id, false);
        });
        if (claimed.view.path("status").asText().equals(kind.equals("MAIL") ? "DELIVERED" : "EXECUTED")) return claimed.view;
        ObjectNode outcome;
        String state;
        try {
            outcome = Objects.requireNonNull(sender.apply((ObjectNode) claimed.view.path("request").deepCopy()));
            state = kind.equals("MAIL") ? "DELIVERED" : "EXECUTED";
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

    private Row map(ResultSet rs, int index) throws SQLException {
        ObjectNode view = mapper.createObjectNode().put("id", rs.getString("id")).put("commandType", rs.getString("command_type")).put("status", rs.getString("status"))
                .put("operator", new String(rs.getBytes("operator_name"), StandardCharsets.UTF_8))
                .put("playerId", rs.getString("player_id")).put("reason", rs.getString("reason"))
                .put("createdAt", Instant.ofEpochMilli(rs.getLong("created_ms")).toString())
                .put("updatedAt", Instant.ofEpochMilli(rs.getLong("updated_ms")).toString());
        view.set("request", parse(rs.getString("request_json")));
        if (view.path("commandType").asText().equals("PLAYER")) view.put("operation", view.path("request").path("operation").asText());
        String result = rs.getString("result_json");
        view.set("result", result == null ? mapper.nullNode() : parse(result));
        return new Row(view, parse(rs.getString("input_json")), rs.getLong("attempt"));
    }

    private ObjectNode parse(String json) {
        try { return (ObjectNode) mapper.readTree(json); }
        catch (JsonProcessingException error) { throw new IllegalStateException("Persisted command JSON is invalid", error); }
    }

    private void audit(String id, String operator, String event, long attempt, ObjectNode result, long now) {
        jdbc.update("INSERT INTO gm_command_audit (command_id,operator_name,event,attempt,result_json,created_ms) VALUES (?,?,?,?,?,?)",
                id, bytes(operator), event, attempt, result == null ? null : result.toString(), now);
    }

    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }
    private static void check(boolean allowed, String message) {
        if (!allowed) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, message);
    }
    private static ResponseStatusException conflict(String message) { return new ResponseStatusException(HttpStatus.CONFLICT, message); }
    private record Row(ObjectNode view, ObjectNode original, long attempt) { }
}
