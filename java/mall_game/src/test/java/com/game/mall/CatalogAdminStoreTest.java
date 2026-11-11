package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.function.IntFunction;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;

/** 每例使用CommerceFixture随机独立库；显式commerce.it.tidb时沿用相同接口和断言。 */
class CatalogAdminStoreTest {
    private static final String ACTOR = "catalog-admin";
    private static final Clock CLOCK = Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC);

    @Test void 同请求并发与后续重放返回原响应_新增和库存差额各只提交一次() throws Exception {
        try (var f = new CommerceFixture()) {
            var replicas = List.of(admin(f), admin(f));
            var create = new CatalogAdminApi.Create("create-once-001", 1, terms(125, 10, 2, "旧条款"), 7);
            var created = parallel(4, index -> replicas.get(index % 2).create(ACTOR, create));
            var first = created.getFirst();
            for (var response : created) { assertEquals(first, response); }
            assertEquals(1, first.skuId());
            assertEquals(1, first.revision());
            assertEquals(7, first.stock());
            assertEquals(1, f.count("pms_sku_stock"));
            assertEquals(1, f.count("commerce_sku_grant"));
            assertEquals(1, f.count("commerce_admin_command"));

            var increase = new CatalogAdminApi.AdjustStock("stock-once-001", 1, 1, 3);
            var increased = parallel(4, index -> replicas.get(index % 2).adjustStock(ACTOR, increase));
            var original = increased.getFirst();
            for (var response : increased) { assertEquals(original, response); }
            assertEquals(2, original.revision());
            assertEquals(10, original.stock());
            assertEquals(2, f.count("commerce_admin_command"));
            assertEquals(new ObjectMapper().writeValueAsString(original), journalResult(f, increase.requestId()));

            var changed = replicas.getFirst().update(ACTOR,
                new CatalogAdminApi.Update("terms-later-001", 1, 2, terms(999, 20, 3, "新条款")));
            var latest = replicas.getLast().adjustStock(ACTOR,
                new CatalogAdminApi.AdjustStock("stock-later-001", 1, changed.revision(), -2));
            assertEquals(8, latest.stock());
            assertEquals(4, latest.revision());
            // 重启后的旧请求仍返回已提交快照，不能把当前修订或新价格拼进旧响应。
            var restarted = admin(f);
            assertEquals(original, restarted.adjustStock(ACTOR, increase));
            assertEquals(first, restarted.create(ACTOR, create));
            assertEquals(latest, sku(restarted, 1));
            assertEquals(4, f.count("commerce_admin_command"));
            assertEquals(0, f.count("oms_order"));
        }
    }

    @Test void 同主体请求号换载荷或命令种类拒绝_日志与业务原件不变() {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            var create = new CatalogAdminApi.Create("fixed-request-001", 1, terms(100, 10, 1, "原条款"), 5);
            store.create(ACTOR, create);
            var before = businessState(f);
            var commands = journalRows(f);
            refuses(409, () -> store.create(ACTOR,
                new CatalogAdminApi.Create(create.requestId(), 1, create.terms(), 6)));
            refuses(409, () -> store.create(ACTOR,
                new CatalogAdminApi.Create(create.requestId(), 1, terms(101, 10, 1, "原条款"), 5)));
            refuses(409, () -> store.adjustStock(ACTOR,
                new CatalogAdminApi.AdjustStock(create.requestId(), 1, 1, 1)));
            refuses(409, () -> store.update(ACTOR,
                new CatalogAdminApi.Update(create.requestId(), 1, 1, create.terms())));
            assertEquals(before, businessState(f));
            assertEquals(commands, journalRows(f));
            assertEquals(1, f.count("commerce_admin_command"));
        }
    }

    @Test void 两个管理副本竞争同修订只有一个提交_失败请求不遗留半完成日志() throws Exception {
        try (var f = new CommerceFixture()) {
            var replicas = List.of(admin(f), admin(f));
            replicas.getFirst().create(ACTOR,
                new CatalogAdminApi.Create("race-create-001", 1, terms(100, 10, 1, "初始"), 10));
            var commands = List.of(
                new CatalogAdminApi.Update("race-update-001", 1, 1, terms(200, 20, 2, "竞争甲")),
                new CatalogAdminApi.Update("race-update-002", 1, 1, terms(300, 30, 3, "竞争乙")));
            var responses = parallel(2, index -> {
                try { return replicas.get(index).update(ACTOR, commands.get(index)); }
                catch (CommerceException conflict) { assertEquals(409, conflict.status()); return null; }
            });
            var winners = responses.stream().filter(value -> value != null).toList();
            assertEquals(1, winners.size());
            var winner = winners.getFirst();
            assertEquals(2, winner.revision());
            assertEquals(10, winner.stock());
            assertEquals(0, winner.lockedStock());
            assertEquals(winner, sku(admin(f), 1));
            assertEquals(2, f.count("commerce_admin_command"));
            int loser = responses.getFirst() == null ? 0 : 1;
            assertEquals(0, journalCount(f, commands.get(loser).requestId()));
            var retried = replicas.get(loser).update(ACTOR,
                new CatalogAdminApi.Update(commands.get(loser).requestId(), 1, 2, commands.get(loser).terms()));
            assertEquals(3, retried.revision());
            assertEquals(commands.get(loser).terms().priceMinor(), retried.priceMinor());
            assertEquals(3, f.count("commerce_admin_command"));
        }
    }

    @Test void 新价格和发物映射只影响新订单_保留旧冻结值及提交时库存预占() {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            store.create(ACTOR, new CatalogAdminApi.Create("freeze-create-001", 1, terms(125, 10, 2, "旧玄铁"), 20));
            var first = f.store.create("1", new CommerceApi.CreateOrder("frozen-order-001", 1, 2, "测试账号", "1"));
            var frozenGrant = f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", first.orderId());
            var frozenItem = f.jdbc.queryForMap("SELECT * FROM oms_order_item WHERE order_sn=?", first.orderId());
            assertEquals(250, first.amountMinor());
            assertEquals("CNY", first.currency());
            assertEquals(10, ((Number) frozenGrant.get("item_config_id")).intValue());
            assertEquals(2, ((Number) frozenGrant.get("count_per_unit")).intValue());
            assertEquals("旧玄铁", frozenGrant.get("title"));
            assertEquals(100, ((Number) frozenItem.get("product_id")).longValue());
            assertEquals("test-sku", frozenItem.get("product_sku_code"));
            assertEquals(125, ((Number) frozenItem.get("product_price_minor")).longValue());
            assertEquals(2, ((Number) frozenItem.get("product_quantity")).intValue());
            assertEquals(1, sku(store, 1).revision(), "普通下单不增加管理修订");

            var nextTerms = new CatalogAdminApi.Terms(200, "new-sku", 999, "USD", true, 20, 3, "新玄铁");
            var changed = store.update(ACTOR, new CatalogAdminApi.Update("freeze-update-001", 1, 1, nextTerms));
            assertEquals(20, changed.stock());
            assertEquals(2, changed.lockedStock());
            assertEquals(2, changed.revision());
            assertEquals(first, f.store.get("1", new CommerceApi.OwnedOrder(first.orderId(), "测试账号", "1")));
            assertEquals(frozenGrant, f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", first.orderId()));
            assertEquals(frozenItem, f.jdbc.queryForMap("SELECT * FROM oms_order_item WHERE order_sn=?", first.orderId()));

            var second = f.store.create("2", new CommerceApi.CreateOrder("future-order-001", 1, 1, "测试账号", "1"));
            assertEquals(999, second.amountMinor());
            assertEquals("USD", second.currency());
            assertEquals(20, f.jdbc.queryForObject("SELECT item_config_id FROM commerce_order_grant WHERE order_sn=?", Integer.class, second.orderId()));
            assertEquals(3, f.jdbc.queryForObject("SELECT count_per_unit FROM commerce_order_grant WHERE order_sn=?", Integer.class, second.orderId()));
            var adjusted = store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("freeze-stock-001", 1, 2, -4));
            assertEquals(16, adjusted.stock());
            assertEquals(3, adjusted.lockedStock(), "差额调整不能抹掉两个原订单的预占");
            var paid = f.store.recordTrustedPayment(new CommerceApi.Payment(first.orderId(), "test", "frozen-pay-001", 250, "CNY")).order();
            assertEquals("DELIVERY_PENDING", paid.state());
            assertEquals(250, paid.amountMinor());
            assertEquals("CNY", paid.currency());
            assertEquals(14, sku(store, 1).stock());
            assertEquals(1, sku(store, 1).lockedStock());
            assertEquals(3, sku(store, 1).revision(), "付款扣库存不增加管理修订");
            assertEquals(frozenGrant, f.jdbc.queryForMap("SELECT * FROM commerce_order_grant WHERE order_sn=?", first.orderId()));
            assertEquals(frozenItem, f.jdbc.queryForMap("SELECT * FROM oms_order_item WHERE order_sn=?", first.orderId()));
        }
    }

    @Test void 库存不能侵占当前预占或越过整数上界_失败回滚管理请求行() {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            store.create(ACTOR, new CatalogAdminApi.Create("bounds-create-001", 1, terms(100, 10, 1, "库存下界"), 5));
            f.store.create("1", new CommerceApi.CreateOrder("bounds-order-001", 1, 3, "测试账号", "1"));
            var before = businessState(f);
            refuses(409, () -> store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-below-001", 1, 1, -3)));
            assertEquals(before, businessState(f));
            assertEquals(0, journalCount(f, "stock-below-001"));
            var atLock = store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-at-lock-001", 1, 1, -2));
            assertEquals(3, atLock.stock());
            assertEquals(3, atLock.lockedStock());
            var atLockState = businessState(f);
            refuses(409, () -> store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-below-002", 1, 2, -1)));
            assertEquals(atLockState, businessState(f));
            assertEquals(0, journalCount(f, "stock-below-002"));

            store.create(ACTOR, new CatalogAdminApi.Create("upper-create-001", 2,
                terms(CommerceStore.MAX_AMOUNT_MINOR, 10, 4096, "整数上界"), Integer.MAX_VALUE - 1));
            var atMaximum = store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-at-max-001", 2, 1, 1));
            assertEquals(Integer.MAX_VALUE, atMaximum.stock());
            assertEquals(2, atMaximum.revision());
            var maximumState = businessState(f);
            refuses(409, () -> store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-over-max-001", 2, 2, 1)));
            refuses(409, () -> store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("stock-min-delta-001", 2, 2, Integer.MIN_VALUE)));
            refuses(400, () -> store.create(ACTOR, new CatalogAdminApi.Create("price-over-max-001", 3,
                terms(CommerceStore.MAX_AMOUNT_MINOR + 1, 10, 1, "金额越界"), 1)));
            assertEquals(maximumState, businessState(f));
            for (String request : List.of("stock-over-max-001", "stock-min-delta-001", "price-over-max-001")) {
                assertEquals(0, journalCount(f, request));
            }
            assertEquals(4, f.count("commerce_admin_command"));
        }
    }

    @Test void 历史SKU缺发物映射可以查询和修复_修复前拒绝库存命令且不留坏响应() {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            f.jdbc.update("INSERT INTO pms_sku_stock(id,product_id,sku_code,price_minor,currency,stock,lock_stock,published)"
                + " VALUES(1,100,'legacy-sku',100,'CNY',5,0,0)");
            var legacy = sku(store, 1);
            assertFalse(legacy.published());
            assertEquals(0, legacy.itemConfigId());
            var before = businessState(f);
            refuses(409, () -> store.adjustStock(ACTOR, new CatalogAdminApi.AdjustStock("legacy-stock-001", 1, 0, 1)));
            assertEquals(before, businessState(f));
            assertEquals(0, f.count("commerce_admin_command"));
            var repaired = store.update(ACTOR,
                new CatalogAdminApi.Update("legacy-repair-001", 1, 0, terms(100, 10, 1, "补齐发物映射")));
            assertEquals(1, repaired.revision());
            assertEquals(10, repaired.itemConfigId());
            var stock = new CatalogAdminApi.AdjustStock("legacy-stock-001", 1, 1, 1);
            var result = store.adjustStock(ACTOR, stock);
            assertEquals(6, result.stock());
            assertEquals(result, admin(f).adjustStock(ACTOR, stock));
            assertEquals(2, f.count("commerce_admin_command"));
        }
    }

    @Test void 最后合法管理修订可以提交_耗尽或负修订拒绝且不留日志() {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            store.create(ACTOR, new CatalogAdminApi.Create("revision-create-001", 1, terms(100, 10, 1, "原条款"), 5));
            assertEquals(1, f.jdbc.update("UPDATE pms_sku_stock SET catalog_revision=? WHERE id=1", Long.MAX_VALUE - 1));
            var last = store.update(ACTOR,
                new CatalogAdminApi.Update("revision-last-001", 1, Long.MAX_VALUE - 1, terms(200, 20, 2, "最后修订")));
            assertEquals(Long.MAX_VALUE, last.revision());
            var before = businessState(f);
            var commands = journalRows(f);
            refuses(400, () -> store.update(ACTOR,
                new CatalogAdminApi.Update("revision-over-001", 1, Long.MAX_VALUE, terms(300, 30, 3, "不可提交"))));
            refuses(400, () -> store.adjustStock(ACTOR,
                new CatalogAdminApi.AdjustStock("revision-over-002", 1, Long.MAX_VALUE, 1)));
            refuses(400, () -> store.adjustStock(ACTOR,
                new CatalogAdminApi.AdjustStock("revision-negative-001", 1, -1, 1)));
            assertEquals(before, businessState(f));
            assertEquals(commands, journalRows(f));
            assertEquals(2, f.count("commerce_admin_command"));
        }
    }

    @Test void 坏响应日志一律返回服务不可用_不能接管重做或替换原件() throws Exception {
        try (var f = new CommerceFixture()) {
            var store = admin(f);
            store.create(ACTOR, new CatalogAdminApi.Create("journal-create-001", 1, terms(100, 10, 1, "原条款"), 10));
            var command = new CatalogAdminApi.AdjustStock("journal-stock-001", 1, 1, 1);
            var original = store.adjustStock(ACTOR, command);
            var json = new ObjectMapper();
            assertEquals(original, json.readValue(journalResult(f, command.requestId()), CatalogAdminApi.Sku.class));
            ObjectNode snapshot = json.valueToTree(original);
            var corruptions = List.of("{", "null", "{}", "pending:" + UUID.randomUUID(),
                snapshot.toString() + " garbage", snapshot.toString() + " {}",
                snapshot.deepCopy().put("skuId", 2).toString(),
                snapshot.deepCopy().put("revision", 0).toString(),
                snapshot.deepCopy().put("stock", -1).toString(),
                snapshot.deepCopy().put("lockedStock", 12).toString(),
                snapshot.deepCopy().put("priceMinor", 0).toString(),
                snapshot.deepCopy().put("countPerUnit", 0).toString());
            var before = businessState(f);
            for (String corrupt : corruptions) {
                assertEquals(1, f.jdbc.update("UPDATE commerce_admin_command SET result_json=? WHERE actor=? AND request_id=?",
                    corrupt, ascii(ACTOR), ascii(command.requestId())));
                var journal = journalRows(f);
                refuses(503, () -> admin(f).adjustStock(ACTOR, command));
                assertEquals(corrupt, journalResult(f, command.requestId()));
                assertEquals(journal, journalRows(f));
                assertEquals(before, businessState(f));
                assertEquals(2, f.count("commerce_admin_command"));
            }
        }
    }

    private static CatalogAdminStore admin(CommerceFixture f) {
        return new CatalogAdminStore(f.jdbc, f.manager, new ObjectMapper(), CLOCK);
    }

    private static CatalogAdminApi.Terms terms(long price, int item, int count, String title) {
        return new CatalogAdminApi.Terms(100, "test-sku", price, "CNY", true, item, count, title);
    }

    private static CatalogAdminApi.Sku sku(CatalogAdminStore store, long id) {
        var rows = store.list(new DeliveryApi.SkuPage(id - 1, 1)).skus();
        assertEquals(1, rows.size());
        assertEquals(id, rows.getFirst().skuId());
        return rows.getFirst();
    }

    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }

    private static int journalCount(CommerceFixture f, String request) {
        return f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_admin_command WHERE actor=? AND request_id=?",
            Integer.class, ascii(ACTOR), ascii(request));
    }

    private static String journalResult(CommerceFixture f, String request) {
        return f.jdbc.queryForObject("SELECT result_json FROM commerce_admin_command WHERE actor=? AND request_id=?",
            String.class, ascii(ACTOR), ascii(request));
    }

    private static List<Map<String, Object>> journalRows(CommerceFixture f) {
        return f.jdbc.queryForList("SELECT command_type,request_digest,result_json,created_ms FROM commerce_admin_command ORDER BY actor,request_id");
    }

    private static Map<String, List<Map<String, Object>>> businessState(CommerceFixture f) {
        // 避开VARBINARY请求键的Java数组对象身份；比较实际持久业务列和冻结合同。
        return Map.of(
            "sku", f.jdbc.queryForList("SELECT * FROM pms_sku_stock ORDER BY id"),
            "skuGrant", f.jdbc.queryForList("SELECT * FROM commerce_sku_grant ORDER BY sku_id"),
            "order", f.jdbc.queryForList("SELECT id,order_sn,buyer_id,sku_id,quantity,pay_amount,currency,status,"
                + "create_time_ms,payment_deadline_ms,payment_time_ms FROM oms_order ORDER BY id"),
            "item", f.jdbc.queryForList("SELECT * FROM oms_order_item ORDER BY order_sn"),
            "orderGrant", f.jdbc.queryForList("SELECT * FROM commerce_order_grant ORDER BY order_sn"));
    }

    private static void refuses(int status, Executable command) {
        assertEquals(status, assertThrows(CommerceException.class, command).status());
    }

    private static <T> List<T> parallel(int count, IntFunction<T> command) throws Exception {
        var executor = Executors.newFixedThreadPool(count);
        var ready = new CountDownLatch(count);
        var start = new CountDownLatch(1);
        List<Future<T>> futures = new ArrayList<>();
        List<T> results = new ArrayList<>();
        List<Executable> failures = new ArrayList<>();
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(20);
        try {
            for (int i = 0; i < count; i++) {
                int index = i;
                futures.add(executor.submit(() -> {
                    ready.countDown();
                    if (!start.await(5, TimeUnit.SECONDS)) { throw new AssertionError("并发起跑门超时"); }
                    return command.apply(index);
                }));
            }
            assertTrue(ready.await(5, TimeUnit.SECONDS), "本例并发任务未全部就绪");
            start.countDown();
            for (var future : futures) {
                try { results.add(future.get(Math.max(1, deadline - System.nanoTime()), TimeUnit.NANOSECONDS)); }
                catch (ExecutionException failed) {
                    Throwable cause = failed.getCause();
                    failures.add(() -> { throw cause; });
                } catch (TimeoutException timedOut) {
                    future.cancel(true);
                    failures.add(() -> { throw timedOut; });
                }
            }
            assertAll("主线程收集全部并发断言与执行错误", failures);
            return results;
        } finally {
            start.countDown();
            for (var future : futures) { if (!future.isDone()) { future.cancel(true); } }
            executor.shutdownNow();
            assertTrue(executor.awaitTermination(5, TimeUnit.SECONDS), "本例executor未在期限内清理");
        }
    }
}
