package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.core.io.ClassPathResource;
import org.springframework.dao.DataAccessException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.DriverManagerDataSource;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;

class GmCommandJournalTest {
    final ObjectMapper mapper = new ObjectMapper();
    final AtomicLong now = new AtomicLong(1_800_000_000_000L);
    final AtomicLong ids = new AtomicLong(100);
    JdbcTemplate jdbc;
    GmCommandJournal journal;
    Clock clock;
    DataSourceTransactionManager manager;

    @BeforeEach void setUp() {
        var data = new DriverManagerDataSource("jdbc:h2:mem:" + UUID.randomUUID() + ";MODE=MySQL;DB_CLOSE_DELAY=-1", "sa", "");
        new ResourceDatabasePopulator(new ClassPathResource("gm-schema.sql")).execute(data);
        jdbc = new JdbcTemplate(data);
        manager = new DataSourceTransactionManager(data);
        clock = new Clock() {
            public ZoneId getZone() { return ZoneOffset.UTC; }
            public Clock withZone(ZoneId zone) { return this; }
            public Instant instant() { return Instant.ofEpochMilli(now.get()); }
        };
        journal = new GmCommandJournal(jdbc, mapper, manager, clock, ids::incrementAndGet);
    }

    ObjectNode input() {
        ObjectNode request = mapper.createObjectNode().put("validDays", 7).put("senderKind", 1)
                .put("title", "测试补偿").put("body", "独立测试");
        request.putObject("attachments").putArray("currencies").addObject().put("currencyType", 0).put("amount", "9007199254740993");
        return request;
    }
    ObjectNode prepare(String key) { return journal.prepare("operator", key, "9007199254740993", "测试原因", input()); }
    String status(ObjectNode command) { return command.path("status").asText(); }
    ObjectNode playerInput() {
        ObjectNode input = mapper.createObjectNode().put("operation", "currency.add");
        input.putObject("arguments").put("currencyType", 0).put("amount", "9007199254740993");
        return input;
    }
    void http(int code, Runnable call) {
        assertEquals(code, assertThrows(ResponseStatusException.class, call::run).getStatusCode().value());
    }
    void await(CountDownLatch latch) {
        try { assertTrue(latch.await(5, TimeUnit.SECONDS)); }
        catch (InterruptedException error) { Thread.currentThread().interrupt(); throw new AssertionError(error); }
    }

    @Test void prepareFreezesExpiryAndStringIdsAndSurvivesReload() {
        ObjectNode original = prepare("same-key");
        now.addAndGet(86_400_000L);
        journal = new GmCommandJournal(jdbc, mapper, manager, clock, ids::incrementAndGet);
        assertEquals(original, prepare("same-key"));
        assertEquals("9007199254740993", original.path("request").path("recipientPlayerId").asText());
        assertTrue(original.path("request").path("expireMs").isTextual());
        assertEquals(original.path("id"), original.path("request").path("requestId"));
        assertFalse(original.path("request").has("validDays"));
        assertEquals(1, journal.list().path("total").asInt());
        assertEquals(original, journal.list().path("records").get(0));
        assertEquals(1, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
    }

    @Test void playerCommandFreezesIdentityAndDoesNotCrossMailEndpoint() {
        ObjectNode draft = journal.preparePlayerCommand("operator", "player-key", "9007199254740993", "客服工单", playerInput());
        String id = draft.path("id").asText();
        assertEquals("PLAYER", draft.path("commandType").asText());
        assertEquals("operator", draft.path("request").path("operator").asText());
        assertEquals("客服工单", draft.path("request").path("reason").asText());
        assertTrue(draft.path("request").path("issuedAtMs").isTextual());
        assertEquals(0, journal.list().path("total").asInt());
        assertEquals(1, journal.listPlayerCommands().path("total").asInt());
        http(409, () -> journal.send(id, "operator", request -> { fail("Wrong transport was called"); return null; }));
        var calls = new AtomicInteger();
        ObjectNode done = journal.sendPlayerCommand(id, "operator", request -> { calls.incrementAndGet(); return mapper.createObjectNode().put("executed", true); });
        assertEquals("EXECUTED", status(done));
        assertEquals(done, journal.sendPlayerCommand(id, "operator", request -> { fail("Executed command was resent"); return null; }));
        assertEquals(1, calls.get());
    }

    @Test void unknownPlayerCommandCannotBeResentAfterTimeoutOrRestart() {
        String id = journal.preparePlayerCommand("operator", "unknown-player", "123", "工单", playerInput()).path("id").asText();
        ObjectNode result = journal.sendPlayerCommand(id, "operator", request -> { throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "unknown", true, 0); });
        assertEquals("UNKNOWN", status(result));
        now.addAndGet(86_400_000);
        journal = new GmCommandJournal(jdbc, mapper, manager, clock, ids::incrementAndGet);
        http(409, () -> journal.sendPlayerCommand(id, "operator", request -> { fail("Unknown command was resent"); return null; }));
        jdbc.update("UPDATE gm_command SET status='SENDING' WHERE id=?", id);
        http(409, () -> journal.sendPlayerCommand(id, "operator", request -> { fail("Interrupted command was resent"); return null; }));
    }

    @Test void idempotencyKeyCannotChangePayloadAndIsCaseSensitive() {
        prepare("key");
        http(409, () -> journal.prepare("operator", "key", "123", "测试原因", input()));
        http(409, () -> journal.prepare("operator", "key", "9007199254740993", "其他原因", input()));
        http(409, () -> journal.prepare("operator", "key", "9007199254740993", "测试原因", input().put("title", "不同内容")));
        journal.prepare("Operator", "key", "9007199254740993", "测试原因", input());
        prepare("KEY");
        assertEquals(3, journal.list().path("total").asInt());
    }

    @Test void concurrentPrepareCreatesOneFrozenCommandAndAudit() throws Exception {
        var pool = Executors.newFixedThreadPool(6);
        try {
            var ready = new CountDownLatch(6);
            var start = new CountDownLatch(1);
            var tasks = new ArrayList<java.util.concurrent.Future<ObjectNode>>();
            for (int i = 0; i < 6; i++) tasks.add(pool.submit(() -> { ready.countDown(); await(start); return prepare("parallel"); }));
            await(ready); start.countDown();
            ObjectNode first = tasks.get(0).get(5, TimeUnit.SECONDS);
            for (var task : tasks) assertEquals(first, task.get(5, TimeUnit.SECONDS));
            assertEquals(1, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
        } finally { pool.shutdownNow(); }
    }

    @Test void onlyOwnerMaySendAndDeliveryReplayDoesNotCallNetwork() {
        String id = prepare("owner").path("id").asText();
        http(403, () -> journal.send(id, "Operator", request -> fail("must not send")));
        AtomicInteger calls = new AtomicInteger();
        ObjectNode delivered = journal.send(id, "operator", request -> {
            calls.incrementAndGet();
            assertFalse(TransactionSynchronizationManager.isActualTransactionActive());
            assertEquals("SENDING", status(journal.get(id)));
            assertEquals(2, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
            return mapper.createObjectNode().put("mailId", "9007199254740999");
        });
        assertEquals("DELIVERED", status(delivered));
        assertEquals(delivered, journal.send(id, "operator", request -> { calls.incrementAndGet(); return request; }));
        assertEquals(1, calls.get());
        assertEquals(3, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
    }

    @Test void uncertainResultPreservesRequestAndNoSensitiveFailureMessage() {
        ObjectNode draft = prepare("unknown");
        String id = draft.path("id").asText();
        ObjectNode unknown = journal.send(id, "operator", request -> { throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "secret-value", true, 12); });
        assertEquals("UNKNOWN", status(unknown));
        assertFalse(unknown.toString().contains("secret-value"));
        now.addAndGet(10_000);
        ObjectNode done = journal.send(id, "operator", request -> {
            assertEquals(draft.path("request"), request);
            return mapper.createObjectNode().put("mailId", "987");
        });
        assertEquals("DELIVERED", status(done));
        assertEquals(5, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
    }

    @Test void concurrentSendIsSingleFlightAndLateOldAttemptCannotOverwriteRetry() throws Exception {
        String id = prepare("lease").path("id").asText();
        var pool = Executors.newSingleThreadExecutor();
        var entered = new CountDownLatch(1);
        var release = new CountDownLatch(1);
        try {
            var first = pool.submit(() -> journal.send(id, "operator", request -> {
                entered.countDown(); await(release);
                throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "unknown", true, 0);
            }));
            await(entered);
            http(409, () -> journal.send(id, "operator", request -> fail("already sending")));
            now.addAndGet(60_001);
            ObjectNode retried = journal.send(id, "operator", request -> mapper.createObjectNode().put("mailId", "222"));
            release.countDown();
            assertEquals(retried, first.get(5, TimeUnit.SECONDS));
            assertEquals("DELIVERED", status(journal.get(id)));
            assertEquals(4, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
        } finally { release.countDown(); pool.shutdownNow(); }
    }

    @Test void failedFinalAuditRollsBackResultAndLeavesSendingIntent() {
        String id = prepare("durability").path("id").asText();
        jdbc.execute("ALTER TABLE gm_command_audit ADD CONSTRAINT no_delivered CHECK (event <> 'DELIVERED')");
        assertThrows(DataAccessException.class, () -> journal.send(id, "operator", request -> mapper.createObjectNode().put("mailId", "123")));
        assertEquals("SENDING", status(journal.get(id)));
        assertTrue(journal.get(id).path("result").isNull());
        assertEquals(2, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit", Integer.class));
    }

    @Test void knownRejectionIsTerminalAndUnknownExceptionStaysUncertain() {
        String rejected = prepare("rejected").path("id").asText();
        assertEquals("REJECTED", status(journal.send(rejected, "operator", request -> { throw new GameServices.CallFailure("NOT_CONFIGURED", "private", false, 0); })));
        http(409, () -> journal.send(rejected, "operator", request -> fail("rejected")));
        String unknown = prepare("runtime").path("id").asText();
        assertEquals("UNKNOWN", status(journal.send(unknown, "operator", request -> { throw new IllegalStateException("private"); })));
        assertFalse(journal.get(unknown).toString().contains("private"));
    }

    @Test void expiredPlayerDraftFailsAtomicallyWithoutTransitioningOrAuditing() {
        String id = journal.preparePlayerCommand("operator", "expiring", "123", "工单", playerInput()).path("id").asText();
        now.addAndGet(GmCommandJournal.PLAYER_DRAFT_TTL_MS + 1);
        http(409, () -> journal.sendPlayerCommand(id, "operator", request -> { fail("Expired draft must not reach sender"); return null; }));
        assertEquals("DRAFT", status(journal.get(id)));
        assertEquals(1, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit WHERE command_id=?", Integer.class, id));
    }

    @Test void zeroIdDrawAndPrimaryKeyCollisionRetryCleanly() {
        var draws = new AtomicInteger();
        long[] sequence = {0L, 500L, 500L, 501L};
        journal = new GmCommandJournal(jdbc, mapper, manager, clock, () -> sequence[draws.getAndIncrement()]);
        ObjectNode first = prepare("k1");
        assertEquals("500", first.path("id").asText());
        ObjectNode second = prepare("k2");
        assertEquals("501", second.path("id").asText());
    }

    @Test void paginationFilteringAndAuditTimelineWorkAcrossCommandTypes() {
        String m1 = journal.prepare("operator", "m1", "1001", "r1", input()).path("id").asText();
        now.addAndGet(1000);
        String m2 = journal.prepare("operator", "m2", "1002", "r2", input()).path("id").asText();
        now.addAndGet(1000);
        String m3 = journal.prepare("operator", "m3", "1001", "r3", input()).path("id").asText();
        journal.send(m2, "operator", request -> { throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "timeout", true, 0); });
        journal.send(m2, "operator", request -> mapper.createObjectNode().put("mailId", "777"));

        ObjectNode page1 = journal.list(1, 2, null, null);
        assertEquals(3, page1.path("total").asInt());
        assertEquals(2, page1.path("records").size());
        assertEquals(m3, page1.path("records").get(0).path("id").asText());
        ObjectNode page2 = journal.list(2, 2, null, null);
        assertEquals(1, page2.path("records").size());
        assertEquals(m1, page2.path("records").get(0).path("id").asText());

        ObjectNode filteredPlayer = journal.list(1, 10, "1001", "DRAFT");
        assertEquals(2, filteredPlayer.path("total").asInt());
        ObjectNode filteredDelivered = journal.list(1, 10, null, "DELIVERED");
        assertEquals(1, filteredDelivered.path("total").asInt());
        assertEquals(m2, filteredDelivered.path("records").get(0).path("id").asText());

        http(400, () -> journal.list(0, 10, null, null));
        http(400, () -> journal.list(1, 101, null, null));
        http(400, () -> journal.list(1, 10, "bad-id", null));
        http(400, () -> journal.list(1, 10, null, "EXECUTED"));

        ObjectNode history = journal.auditHistory(m2, "MAIL");
        assertEquals(m2, history.path("commandId").asText());
        assertEquals(5, history.path("records").size());
        assertEquals("DRAFT", history.path("records").get(0).path("event").asText());
        assertEquals("UNKNOWN", history.path("records").get(2).path("event").asText());
        assertEquals("DELIVERED", history.path("records").get(4).path("event").asText());
        http(409, () -> journal.auditHistory(m2, "PLAYER"));
        assertEquals(m2, journal.get(m2, "MAIL").path("id").asText());
        http(409, () -> journal.get(m2, "PLAYER"));
    }

    @Test void expiredMailDraftFailsAtomicallyBeforeTransitioning() {
        String id = journal.prepare("operator", "expired-mail", "1001", "过期测试", input().put("validDays", 1)).path("id").asText();
        now.addAndGet(86_400_000L);
        http(409, () -> journal.send(id, "operator", request -> { fail("Expired mail draft must not send"); return null; }));
        assertEquals("DRAFT", status(journal.get(id)));
        assertEquals(1, jdbc.queryForObject("SELECT COUNT(*) FROM gm_command_audit WHERE command_id=?", Integer.class, id));
    }
}
