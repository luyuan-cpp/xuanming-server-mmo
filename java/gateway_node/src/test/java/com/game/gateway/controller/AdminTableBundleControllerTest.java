package com.game.gateway.controller;

import com.game.gateway.service.TablePointerStore;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.web.server.ResponseStatusException;

import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.TreeMap;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 配置表热更管理接口契约测试(docs/design/config-table-hot-reload.md §6.4):
 * 验证轨道校验、activate_at 门槛、current/next 折叠、etcd CAS 冲突、撤销与删线守卫、状态聚合。
 */
class AdminTableBundleControllerTest {

    private static final String SCHEMA = "0123456789abcdef";
    private static final String DIGEST_V1 = "1".repeat(64);
    private static final String DIGEST_V2 = "2".repeat(64);
    private static final String SHA_V1 = "a".repeat(64);
    private static final String SHA_V2 = "b".repeat(64);

    static final class MemoryStore implements TablePointerStore {
        final Map<String, Entry> kv = new TreeMap<>();
        final AtomicLong rev = new AtomicLong(1);
        boolean forceCasConflict;

        @Override
        public Optional<Entry> get(String key) {
            return Optional.ofNullable(kv.get(key));
        }

        @Override
        public Map<String, Entry> list(String prefix) {
            Map<String, Entry> out = new TreeMap<>();
            kv.forEach((k, v) -> {
                if (k.startsWith(prefix)) {
                    out.put(k, v);
                }
            });
            return out;
        }

        @Override
        public boolean compareAndPut(String key, long expectedModRevision, String value) {
            if (forceCasConflict) {
                return false;
            }
            Entry cur = kv.get(key);
            long curRev = cur == null ? 0L : cur.modRevision();
            if (curRev != expectedModRevision) {
                return false;
            }
            kv.put(key, new Entry(value, rev.incrementAndGet()));
            return true;
        }

        @Override
        public boolean compareAndDelete(String key, long expectedModRevision) {
            if (forceCasConflict) {
                return false;
            }
            Entry cur = kv.get(key);
            if (cur == null || cur.modRevision() != expectedModRevision) {
                return false;
            }
            kv.remove(key);
            return true;
        }
    }

    static final class MutableClock extends Clock {
        private long millis;

        MutableClock(long millis) {
            this.millis = millis;
        }

        void advanceMs(long delta) {
            this.millis += delta;
        }

        @Override
        public ZoneOffset getZone() {
            return ZoneOffset.UTC;
        }

        @Override
        public Clock withZone(java.time.ZoneId zone) {
            return this;
        }

        @Override
        public Instant instant() {
            return Instant.ofEpochMilli(millis);
        }
    }

    private MemoryStore store;
    private MutableClock clock;
    private AdminTableBundleController controller;
    private MockHttpServletRequest request;

    @BeforeEach
    void setUp() {
        store = new MemoryStore();
        clock = new MutableClock(1_800_000_000_000L);
        controller = new AdminTableBundleController(store, clock);
        request = new MockHttpServletRequest();
        request.addHeader(AdminTableBundleController.OPERATOR_HEADER, "ops.alice");
    }

    private static HttpStatus statusOf(Runnable action) {
        ResponseStatusException e = assertThrows(ResponseStatusException.class, action::run);
        return HttpStatus.valueOf(e.getStatusCode().value());
    }

    @Test
    void publishFoldsActiveNextIntoCurrentAndRejectsTooSoonActivateAt() {
        // 非 immediate 且不足 60 秒被拒。
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.publish(
                new AdminTableBundleController.PublishRequest("stable", SCHEMA, DIGEST_V1, SHA_V1, 1L,
                        clock.millis() + 30_000L, false, "too soon"),
                request)));

        // 非法轨道被拒。
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.publish(
                new AdminTableBundleController.PublishRequest("zone-1", SCHEMA, DIGEST_V1, SHA_V1, 1L,
                        null, true, "bad track"),
                request)));

        // 首次发布 v1(60 秒后生效)。
        var v1 = controller.publish(new AdminTableBundleController.PublishRequest(
                "stable", SCHEMA, DIGEST_V1, SHA_V1, 1L, clock.millis() + 60_000L, false, "release v1"), request);
        assertNull(v1.current());
        assertEquals(DIGEST_V1, v1.next().contentDigest());
        assertNull(v1.active());

        // 时钟推进 61 秒后发布 v2:v1 已生效,应折进 current。
        clock.advanceMs(61_000L);
        var v2 = controller.publish(new AdminTableBundleController.PublishRequest(
                "stable", SCHEMA, DIGEST_V2, SHA_V2, 2L, null, true, "hotfix v2"), request);
        assertNotNull(v2.current());
        assertEquals(DIGEST_V1, v2.current().contentDigest());
        assertEquals(DIGEST_V2, v2.next().contentDigest());
        assertEquals(DIGEST_V2, v2.active().contentDigest());

        // 并发 CAS 冲突返回 409。
        store.forceCasConflict = true;
        assertEquals(HttpStatus.CONFLICT, statusOf(() -> controller.publish(
                new AdminTableBundleController.PublishRequest("stable", SCHEMA, DIGEST_V1, SHA_V1, 3L,
                        null, true, "race"),
                request)));
    }

    @Test
    void cancelNextRejectsAfterActivateAt() {
        long actAt = clock.millis() + 120_000L;
        controller.publish(new AdminTableBundleController.PublishRequest(
                "canary", SCHEMA, DIGEST_V1, SHA_V1, 10L, actAt, false, "test"), request);

        // 到点前可撤销。
        var cancelled = controller.cancelNext("canary", SCHEMA, request);
        assertNull(cancelled.next());

        // 重新发布并推进到生效后:撤销必须回 409。
        controller.publish(new AdminTableBundleController.PublishRequest(
                "canary", SCHEMA, DIGEST_V1, SHA_V1, 11L, clock.millis() + 60_000L, false, "test2"), request);
        clock.advanceMs(61_000L);
        assertEquals(HttpStatus.CONFLICT, statusOf(() -> controller.cancelNext("canary", SCHEMA, request)));
    }

    @Test
    void deleteLineRefusesWhenPodsReportAndListAggregatesStatuses() {
        controller.publish(new AdminTableBundleController.PublishRequest(
                "stable", SCHEMA, DIGEST_V1, SHA_V1, 5L, null, true, "v5"), request);

        store.kv.put("/mmorpg/tables/status/stable/scene-0", new TablePointerStore.Entry(
                "{\"service\":\"scene\",\"schema_digest\":\"" + SCHEMA + "\",\"loaded_digest\":\"" + DIGEST_V1 + "\"}", 10));
        store.kv.put("/mmorpg/tables/status/stable/battle-0", new TablePointerStore.Entry(
                "{\"service\":\"battle\",\"schema_digest\":\"" + SCHEMA + "\",\"loaded_digest\":\"" + DIGEST_V2
                        + "\",\"pending_state\":\"rejected\",\"pending_digest\":\"" + DIGEST_V1
                        + "\",\"pending_reason\":\"invalid skill table\"}", 11));
        store.kv.put("/mmorpg/tables/status/stable/match-0", new TablePointerStore.Entry(
                "{\"service\":\"match\",\"schema_digest\":\"" + SCHEMA + "\",\"loaded_digest\":\"" + DIGEST_V2
                        + "\",\"pending_state\":\"needs_restart\",\"pending_digest\":\"" + DIGEST_V1
                        + "\",\"pending_reason\":\"non-hot-reloadable: ZoneGroup\"}", 12));

        List<AdminTableBundleController.LineView> views = controller.list();
        assertEquals(1, views.size());
        var view = views.get(0);
        assertEquals(3, view.summary().total());
        assertEquals(1, view.summary().loaded());
        assertEquals(1, view.summary().rejected());
        assertEquals(1, view.summary().needsRestart());
        assertEquals(1, view.rejectedPods().size());
        assertEquals("battle-0", view.rejectedPods().get(0).pod());
        assertEquals("invalid skill table", view.rejectedPods().get(0).reason());

        // 有在线 Pod 回报时删除 schema 线必须回 409。
        assertEquals(HttpStatus.CONFLICT, statusOf(() -> controller.deleteLine("stable", SCHEMA, request)));

        // 清空在线回报后删除成功。
        store.kv.remove("/mmorpg/tables/status/stable/scene-0");
        store.kv.remove("/mmorpg/tables/status/stable/battle-0");
        store.kv.remove("/mmorpg/tables/status/stable/match-0");
        var del = controller.deleteLine("stable", SCHEMA, request);
        assertTrue(del.deleted());
        assertTrue(store.get("/mmorpg/tables/pointer/stable/" + SCHEMA).isEmpty());
    }

    private void putStatus(String pod, String loadedDigest, String pendingState, String pendingDigest) {
        store.kv.put("/mmorpg/tables/status/stable/" + pod, new TablePointerStore.Entry(
                "{\"service\":\"scene\",\"schema_digest\":\"" + SCHEMA + "\",\"loaded_digest\":\"" + loadedDigest
                        + "\",\"pending_state\":\"" + pendingState + "\",\"pending_digest\":\"" + pendingDigest
                        + "\",\"pending_reason\":\"unit test\"}", 20));
    }

    // 进程的 pending_* 说的是它当时看到的那一包。指针已经换成 V2,回报还停在 V1 的,
    // 不能算成「V2 被拒绝」:发布工具会据此判 V2 发布失败并撤回。
    @Test
    void listDoesNotBlameTheNewBundleForWhatPodsSaidAboutAnOlderOne() {
        String loadedBefore = "0".repeat(64);
        controller.publish(new AdminTableBundleController.PublishRequest(
                "stable", SCHEMA, DIGEST_V2, SHA_V2, 6L, null, true, "v6"), request);

        putStatus("scene-0", loadedBefore, "rejected", DIGEST_V1);
        putStatus("scene-1", loadedBefore, "needs_restart", DIGEST_V1);
        putStatus("scene-2", loadedBefore, "prepared", DIGEST_V1);
        putStatus("scene-3", DIGEST_V2, "", "");

        var stale = controller.list().get(0);
        assertEquals(4, stale.summary().total());
        assertEquals(1, stale.summary().loaded());
        assertEquals(0, stale.summary().rejected());
        assertEquals(0, stale.summary().needsRestart());
        assertEquals(0, stale.summary().prepared());
        assertTrue(stale.rejectedPods().isEmpty());
        assertTrue(stale.needsRestartPods().isEmpty());

        // 回报跟上来之后照常计数。
        putStatus("scene-0", loadedBefore, "rejected", DIGEST_V2);
        putStatus("scene-1", loadedBefore, "needs_restart", DIGEST_V2);
        putStatus("scene-2", DIGEST_V2, "", "");

        var caughtUp = controller.list().get(0);
        assertEquals(2, caughtUp.summary().loaded());
        assertEquals(1, caughtUp.summary().rejected());
        assertEquals(1, caughtUp.summary().needsRestart());
        assertEquals("scene-0", caughtUp.rejectedPods().get(0).pod());
        assertEquals(DIGEST_V2, caughtUp.rejectedPods().get(0).digest());
    }

    // 各 zone namespace 里的 StatefulSet 同名,sidecar 的状态键是 <轨道>/<namespace>/<pod>(go/table_sync 的 statusIdentity)。
    // 两个 zone 的 scene-0 要算两个实例,其中一个的拒绝不能被另一个的「已加载」盖掉;有实例在回报时删线照样拒绝。
    @Test
    void listCountsSameNamedPodsOfDifferentZonesSeparately() {
        controller.publish(new AdminTableBundleController.PublishRequest(
                "stable", SCHEMA, DIGEST_V2, SHA_V2, 7L, null, true, "v7"), request);

        putStatus("mmorpg-zone-1/scene-0", DIGEST_V2, "", "");
        putStatus("mmorpg-zone-2/scene-0", DIGEST_V1, "rejected", DIGEST_V2);

        var view = controller.list().get(0);
        assertEquals(2, view.summary().total());
        assertEquals(1, view.summary().loaded());
        assertEquals(1, view.summary().rejected());
        assertEquals("mmorpg-zone-2/scene-0", view.rejectedPods().get(0).pod());

        assertEquals(HttpStatus.CONFLICT, statusOf(() -> controller.deleteLine("stable", SCHEMA, request)));
    }
}
