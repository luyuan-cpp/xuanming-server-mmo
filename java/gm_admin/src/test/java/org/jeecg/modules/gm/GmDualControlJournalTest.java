package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.DriverManagerDataSource;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;

/** 开启双人复核的流水:第一次放行必须是另一个人,批准人只记第一次,申请会过期,可以驳回与撤回。 */
class GmDualControlJournalTest {
    private static final long HOUR_MS = 3_600_000L;
    final ObjectMapper mapper = new ObjectMapper();
    final AtomicLong now = new AtomicLong(1_800_000_000_000L);
    final AtomicLong ids = new AtomicLong(100);
    JdbcTemplate jdbc;
    GmCommandJournal journal;

    @BeforeEach void setUp() {
        var data = new DriverManagerDataSource("jdbc:h2:mem:" + UUID.randomUUID() + ";MODE=MySQL;DB_CLOSE_DELAY=-1", "sa", "");
        new ResourceDatabasePopulator(new ClassPathResource("gm-schema.sql")).execute(data);
        jdbc = new JdbcTemplate(data);
        Clock clock = new Clock() {
            public ZoneId getZone() { return ZoneOffset.UTC; }
            public Clock withZone(ZoneId zone) { return this; }
            public Instant instant() { return Instant.ofEpochMilli(now.get()); }
        };
        journal = new GmCommandJournal(jdbc, mapper, new DataSourceTransactionManager(data), clock, ids::incrementAndGet, true, 24);
    }

    ObjectNode mail() {
        ObjectNode request = mapper.createObjectNode().put("validDays", 7).put("senderKind", 1).put("title", "补偿").put("body", "双人复核测试");
        request.putObject("attachments").putArray("currencies").addObject().put("currencyType", 0).put("amount", "100");
        return request;
    }
    String propose(String proposer, String key) {
        return journal.prepare(proposer, key, "9007199254740993", "工单 1024", mail()).path("id").asText();
    }
    ObjectNode delivered() { return mapper.createObjectNode().put("mailId", "9901"); }
    void http(int code, Runnable call) {
        assertEquals(code, assertThrows(ResponseStatusException.class, call::run).getStatusCode().value());
    }
    List<String> auditTrail(String id) {
        List<String> trail = new ArrayList<>();
        for (JsonNode entry : journal.auditHistory(id, "MAIL").path("records")) {
            trail.add(entry.path("event").asText() + ":" + entry.path("operator").asText());
        }
        return trail;
    }

    @Test void theProposerCannotReleaseTheirOwnRequestButAnotherPersonCan() {
        assertTrue(journal.dualControl());
        String id = propose("alice", "k1");
        http(403, () -> journal.send(id, "alice", request -> fail("a self-approved command must never be sent")));
        ObjectNode pending = journal.get(id, "MAIL");
        assertEquals("DRAFT", pending.path("status").asText());
        assertTrue(pending.path("approver").isNull());
        assertTrue(pending.path("approvedAt").isNull());

        now.addAndGet(60_000L);
        ObjectNode done = journal.send(id, "bob", request -> delivered());
        assertEquals("DELIVERED", done.path("status").asText());
        assertEquals("alice", done.path("operator").asText());
        assertEquals("bob", done.path("approver").asText());
        assertEquals(Instant.ofEpochMilli(now.get()).toString(), done.path("approvedAt").asText());
        assertEquals(List.of("DRAFT:alice", "APPROVED:bob", "SENDING:bob", "DELIVERED:bob"), auditTrail(id));
        // 已完成的命令再点一次只回原结果,发起人来点也一样,不会再发。
        assertEquals("DELIVERED", journal.send(id, "alice", request -> fail("already delivered")).path("status").asText());
    }

    @Test void operatorNamesAreComparedByteForByte() {
        // 身份在登录时已经保证一人一个标识;流水只管按字节比较,不做大小写折叠,否则库的比较规则会替我们决定谁和谁是同一个人。
        String id = propose("alice", "k1");
        assertEquals("DELIVERED", journal.send(id, "Alice", request -> delivered()).path("status").asText());
    }

    @Test void aRetryAfterAnUnknownResultKeepsTheFirstApproverAndNeedsNoSecondApproval() {
        String id = propose("alice", "k1");
        ObjectNode unknown = journal.send(id, "bob", request -> { throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "unknown", true, 0); });
        assertEquals("UNKNOWN", unknown.path("status").asText());
        assertEquals("bob", unknown.path("approver").asText());
        String approvedAt = unknown.path("approvedAt").asText();

        // 这条命令已经过了双人;发起人自己用原请求号重试是允许的,批准人与批准时刻不变。
        now.addAndGet(5 * 60_000L);
        ObjectNode retried = journal.send(id, "alice", request -> delivered());
        assertEquals("DELIVERED", retried.path("status").asText());
        assertEquals("bob", retried.path("approver").asText());
        assertEquals(approvedAt, retried.path("approvedAt").asText());
        assertEquals(List.of("DRAFT:alice", "APPROVED:bob", "SENDING:bob", "UNKNOWN:bob", "SENDING:alice", "DELIVERED:alice"), auditTrail(id));
    }

    @Test void anUnreleasedRequestCannotBeApprovedAfterTheApprovalWindow() {
        String id = propose("alice", "k1");
        now.addAndGet(24 * HOUR_MS + 1);
        http(409, () -> journal.send(id, "bob", request -> fail("an expired request must not be sent")));
        assertEquals("DRAFT", journal.get(id, "MAIL").path("status").asText());
        assertTrue(journal.get(id, "MAIL").path("approver").isNull());

        // 恰好在时限内的申请仍可批准。
        String onTime = propose("alice", "k2");
        now.addAndGet(24 * HOUR_MS);
        assertEquals("DELIVERED", journal.send(onTime, "bob", request -> delivered()).path("status").asText());
    }

    @Test void aDeclinedRequestRecordsWhoDeclinedAndCanNeverBeReleased() {
        String id = propose("alice", "k1");
        http(400, () -> journal.decline(id, "bob", " ", "MAIL", false));
        http(409, () -> journal.decline(id, "bob", "金额与工单不符", "SYSTEM_MAIL", false));
        ObjectNode declined = journal.decline(id, "bob", "金额与工单不符", "MAIL", false);
        assertEquals("DECLINED", declined.path("status").asText());
        assertEquals("bob", declined.path("result").path("declinedBy").asText());
        assertEquals("金额与工单不符", declined.path("result").path("reason").asText());
        assertTrue(declined.path("approver").isNull());
        http(409, () -> journal.send(id, "carol", request -> fail("a declined request must never be sent")));
        http(409, () -> journal.decline(id, "carol", "重复驳回", "MAIL", false));
        assertEquals(List.of("DRAFT:alice", "DECLINED:bob"), auditTrail(id));
        assertEquals(1, journal.list(1, 10, null, "DECLINED").path("total").asInt());
        // 同一个幂等号重放拿回的是被驳回的那一条;要再做同一件事必须换号重新发起。
        assertEquals("DECLINED", journal.prepare("alice", "k1", "9007199254740993", "工单 1024", mail()).path("status").asText());
    }

    @Test void someoneWhoCanOnlyProposeMayWithdrawOnlyTheirOwnRequest() {
        String id = propose("alice", "k1");
        http(403, () -> journal.decline(id, "mallory", "不是我的申请", "MAIL", true));
        assertEquals("DRAFT", journal.get(id, "MAIL").path("status").asText());
        assertEquals("DECLINED", journal.decline(id, "alice", "填错了金额", "MAIL", true).path("status").asText());
    }

    @Test void aReleasedRequestCanNoLongerBeDeclined() {
        String id = propose("alice", "k1");
        journal.send(id, "bob", request -> { throw new GameServices.CallFailure("DEADLINE_EXCEEDED", "unknown", true, 0); });
        http(409, () -> journal.decline(id, "carol", "太晚了", "MAIL", false));
        assertEquals("UNKNOWN", journal.get(id, "MAIL").path("status").asText());
    }

    @Test void onlyOneOfTwoConcurrentApproversSendsTheCommand() throws Exception {
        String id = propose("alice", "k1");
        AtomicInteger sent = new AtomicInteger();
        CountDownLatch start = new CountDownLatch(1);
        CountDownLatch finished = new CountDownLatch(2);
        for (String approver : List.of("bob", "carol")) {
            new Thread(() -> {
                try {
                    start.await();
                    journal.send(id, approver, request -> { sent.incrementAndGet(); return delivered(); });
                } catch (ResponseStatusException | InterruptedException lostTheRace) {
                    // 输的一方要么撞上「正在发送」,要么等到对方完成后拿回原结果;两种都不再发。
                } finally {
                    finished.countDown();
                }
            }).start();
        }
        start.countDown();
        assertTrue(finished.await(10, TimeUnit.SECONDS));
        assertEquals(1, sent.get());
        ObjectNode done = journal.get(id, "MAIL");
        assertEquals("DELIVERED", done.path("status").asText());
        assertTrue(List.of("bob", "carol").contains(done.path("approver").asText()));
        assertEquals(1, auditTrail(id).stream().filter(entry -> entry.startsWith("APPROVED:")).count());
    }

    @Test void playerCommandsAlsoNeedASecondPerson() {
        ObjectNode input = mapper.createObjectNode().put("operation", "currency.add");
        input.putObject("arguments").put("currencyType", 0).put("amount", "100");
        String id = journal.preparePlayerCommand("alice", "p1", "9007199254740993", "客服工单", input).path("id").asText();
        http(403, () -> journal.sendPlayerCommand(id, "alice", request -> fail("self-approved")));
        ObjectNode done = journal.sendPlayerCommand(id, "bob", request -> mapper.createObjectNode().put("executed", true));
        assertEquals("EXECUTED", done.path("status").asText());
        assertEquals("bob", done.path("approver").asText());
    }

    @Test void tradeManualCommandsHandTheApprovalRecordToTheSenderAndReuseItOnRetry() {
        ObjectNode input = mapper.createObjectNode().put("commerceId", "42").put("action", "refund").put("expectVersion", "7")
                .put("expectTarget", "staging-global");
        String id = journal.prepareTradeManual("alice", "t1", "工单 77", input).path("id").asText();
        ObjectNode frozen = journal.get(id, "TRADE_MANUAL");
        assertEquals("alice", frozen.path("request").path("operator").asText());
        assertEquals("工单 77", frozen.path("request").path("reason").asText());
        assertEquals("0", frozen.path("playerId").asText());
        http(403, () -> journal.sendTradeManual(id, "alice", (request, approval) -> fail("self-approved")));

        List<ObjectNode> approvals = new ArrayList<>();
        ObjectNode unknown = journal.sendTradeManual(id, "bob", (request, approval) -> {
            approvals.add(approval);
            throw new GameServices.CallFailure("UNAVAILABLE", "unknown", true, 0);
        });
        assertEquals("UNKNOWN", unknown.path("status").asText());
        // 交易人工处置按批准号幂等,结果未知后可以用原命令重试;批准记录原样复用,请求摘要不变。
        now.addAndGet(120_000L);
        ObjectNode done = journal.sendTradeManual(id, "carol", (request, approval) -> {
            approvals.add(approval);
            assertEquals("alice", request.path("operator").asText());
            return mapper.createObjectNode().put("verdict", "MANUAL_CONTRACT_VERDICT_OK");
        });
        assertEquals("EXECUTED", done.path("status").asText());
        assertEquals(2, approvals.size());
        assertEquals(approvals.get(0), approvals.get(1));
        ObjectNode approval = approvals.get(0);
        assertEquals(id, approval.path("approvalId").asText());
        assertEquals("alice", approval.path("proposer").asText());
        assertEquals("bob", approval.path("approver").asText());
        assertEquals(Long.toString(Instant.parse(done.path("approvedAt").asText()).toEpochMilli()), approval.path("approvedAtMs").asText());
    }

    @Test void tradeManualDraftsRejectServerFrozenFields() {
        for (String frozen : List.of("operator", "reason", "mode", "approval", "auth", "requestId")) {
            ObjectNode input = mapper.createObjectNode().put("commerceId", "42").put("action", "refund").put("expectVersion", "7")
                    .put("expectTarget", "staging-global").put(frozen, "x");
            http(400, () -> journal.prepareTradeManual("alice", "t-" + frozen, "工单", input));
        }
        http(400, () -> journal.prepareTradeManual("alice", "t-shape", "工单", mapper.createObjectNode().put("action", "refund")));
    }

    @Test void theSwitchIsAlwaysOnInOidcModeAndOffByDefaultInNativeMode() {
        assertTrue(GmCommandJournal.dualControlEnforced("oidc", ""));
        assertTrue(GmCommandJournal.dualControlEnforced("oidc", "true"));
        assertThrows(IllegalStateException.class, () -> GmCommandJournal.dualControlEnforced("oidc", "false"));
        assertFalse(GmCommandJournal.dualControlEnforced("native", ""));
        assertFalse(GmCommandJournal.dualControlEnforced("native", null));
        assertTrue(GmCommandJournal.dualControlEnforced("native", "true"));
        assertFalse(GmCommandJournal.dualControlEnforced("native", "false"));
        assertThrows(IllegalStateException.class, () -> GmCommandJournal.dualControlEnforced("native", "yes"));
        assertThrows(IllegalStateException.class, () -> GmCommandJournal.dualControlEnforced("sso", ""));
    }
}
