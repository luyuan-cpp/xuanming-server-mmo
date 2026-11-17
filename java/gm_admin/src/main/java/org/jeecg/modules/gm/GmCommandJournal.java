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
import java.util.List;
import java.util.Objects;
import java.util.function.Function;
import java.util.function.LongSupplier;
import org.springframework.beans.factory.annotation.Autowired;
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
public final class GmCommandJournal {
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
        check(operator != null && !operator.isBlank() && bytes(operator).length <= 512, "操作者非法");
        check(clientRequestId != null && clientRequestId.matches("[A-Za-z0-9._:-]{1,128}"), "客户端幂等号非法");
        check(playerId != null && playerId.matches("[1-9][0-9]{0,19}"), "玩家 ID 非法");
        check(reason != null && !reason.isBlank() && reason.length() <= 1000, "操作原因非法");
        check(input != null && input.path("validDays").isIntegralNumber()
                && input.path("validDays").canConvertToInt(), "有效天数非法");
        int days = input.path("validDays").intValue();
        check(days >= 1 && days <= 30, "有效天数须为 1 至 30");
        for (String field : List.of("requestId", "request_id", "expireMs", "expire_ms", "auth", "recipient_player_id")) {
            check(!input.has(field), "不得覆盖服务端冻结字段");
        }
        check(!input.has("recipientPlayerId") || playerId.equals(input.path("recipientPlayerId").asText()), "玩家不一致");
        ObjectNode original = input.deepCopy();
        List<Row> previous = byKey(operator, clientRequestId);
        if (!previous.isEmpty()) return replay(previous.get(0), playerId, reason, original);
        long candidate = ids.getAsLong() & Long.MAX_VALUE;
        if (candidate == 0) throw new IllegalStateException("Command ID generation failed");
        String id = Long.toString(candidate);
        long now = clock.millis();
        ObjectNode request = original.deepCopy();
        request.remove("validDays");
        request.put("requestId", id).put("recipientPlayerId", playerId)
                .put("expireMs", Long.toString(Math.addExact(now, days * 86_400_000L)));
        try {
            return tx.execute(status -> {
                jdbc.update("INSERT INTO gm_command (id,operator_name,client_request_id,player_id,reason,input_json,"
                        + "request_json,status,attempt,created_ms,updated_ms) VALUES (?,?,?,?,?,?,?,'DRAFT',0,?,?)",
                        id, bytes(operator), bytes(clientRequestId), playerId, reason, original.toString(), request.toString(), now, now);
                audit(id, operator, "DRAFT", 0, null, now);
                return get(id);
            });
        } catch (DuplicateKeyException collision) {
            previous = byKey(operator, clientRequestId);
            if (!previous.isEmpty()) return replay(previous.get(0), playerId, reason, original);
            throw conflict("命令号冲突，请重新创建草稿");
        }
    }

    public ObjectNode get(String id) { return row(id, false).view; }

    public ObjectNode list() {
        ObjectNode result = mapper.createObjectNode();
        var records = result.putArray("records");
        jdbc.query("SELECT * FROM gm_command ORDER BY created_ms DESC,id DESC LIMIT 50", this::map)
                .forEach(row -> records.add(row.view));
        result.put("total", jdbc.queryForObject("SELECT COUNT(*) FROM gm_command", Long.class));
        return result;
    }

    public ObjectNode send(String id, String operator, Function<ObjectNode, ObjectNode> sender) {
        Objects.requireNonNull(sender);
        if (TransactionSynchronizationManager.isActualTransactionActive()) {
            throw new IllegalStateException("Sending inside an ambient transaction is forbidden");
        }
        Row claimed = tx.execute(status -> {
            Row current = row(id, true);
            if (!current.view.path("operator").asText().equals(operator)) {
                throw new ResponseStatusException(HttpStatus.FORBIDDEN, "仅草稿创建者可发送");
            }
            if (current.view.path("status").asText().equals("DELIVERED")) return current;
            long now = clock.millis();
            int changed = jdbc.update("UPDATE gm_command SET status='SENDING',attempt=attempt+1,updated_ms=? "
                    + "WHERE id=? AND attempt=? AND (status IN ('DRAFT','UNKNOWN') OR (status='SENDING' AND updated_ms<?))",
                    now, id, current.attempt, now - 60_000);
            if (changed != 1) throw conflict("命令正在发送或已拒绝，请核对当前状态");
            audit(id, operator, "SENDING", current.attempt + 1, null, now);
            return row(id, false);
        });
        if (claimed.view.path("status").asText().equals("DELIVERED")) return claimed.view;
        ObjectNode outcome;
        String state;
        try {
            outcome = Objects.requireNonNull(sender.apply((ObjectNode) claimed.view.path("request").deepCopy()));
            state = "DELIVERED";
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

    private ObjectNode replay(Row old, String player, String reason, ObjectNode original) {
        if (!old.view.path("playerId").asText().equals(player) || !old.view.path("reason").asText().equals(reason)
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
        ObjectNode view = mapper.createObjectNode().put("id", rs.getString("id")).put("status", rs.getString("status"))
                .put("operator", new String(rs.getBytes("operator_name"), StandardCharsets.UTF_8))
                .put("playerId", rs.getString("player_id")).put("reason", rs.getString("reason"))
                .put("createdAt", Instant.ofEpochMilli(rs.getLong("created_ms")).toString())
                .put("updatedAt", Instant.ofEpochMilli(rs.getLong("updated_ms")).toString());
        view.set("request", parse(rs.getString("request_json")));
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
