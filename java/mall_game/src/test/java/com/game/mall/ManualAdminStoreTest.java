package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;

/** 覆盖官方订单、退款待办、人民币寄售与冻结收入的人工单(MANUAL)查询与受控处置。 */
class ManualAdminStoreTest {
    private static final String ACTOR = "catalog-admin";
    private static final Clock CLOCK = Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC);
    private static final String PROOF = "a".repeat(64);
    private static final String ESCROW_PROOF = "b".repeat(64);

    @Test void 官方订单扣回转人工后支持重试扣回与人工终结_幂等重放不重复落库() {
        try (var f = new CommerceFixture()) {
            var manual = manual(f);
            var delivery = delivery(f);
            var revision = revision(f);
            f.sku(1, 10, 125);
            var order = f.store.create("1", new CommerceApi.CreateOrder("manual-rev-001", 1, 1, "buyer-acct", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "pay-rev-001", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "1001", "DELIVERED", PROOF));
            revision.record(new CommerceApi.Revision(order.orderId(), "test", "pay-rev-001", 1, "REVOKED"));
            delivery.reversalReceipt(new DeliveryApi.ReversalReceipt(order.orderId(), 1, "1001", "MANUAL", PROOF));

            var summary = manual.list(new ManualAdminApi.ListQuery("", "", "", 20));
            assertEquals(1, summary.orders().size());
            var row = summary.orders().getFirst();
            assertEquals(order.orderId(), row.orderId());
            assertEquals(1, row.reversalRevision());
            assertEquals("MANUAL", row.reversalStatus());

            // 奇数修订号(已撤销)不能直接标 DELIVERED。
            refuses(409, () -> manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-wrong-001", order.orderId(), "MARK_DELIVERED", "ticket-1")));

            // RETRY_REVERSAL: 清掉 MANUAL 回执、重置 PENDING、订单回 REVERSING(8)，trade 重试后可正常回执 REVERSED。
            var retryCmd = new ManualAdminApi.ResolveOrder("ord-retry-001", order.orderId(), "RETRY_REVERSAL", "ticket-2");
            var retried = manual.resolveOrder(ACTOR, retryCmd);
            assertEquals("REVERSING", retried.resultingState());
            assertEquals(retried, manual.resolveOrder(ACTOR, retryCmd), "同 requestId 重放返回原提交快照");
            refuses(409, () -> manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-retry-001", order.orderId(), "MARK_REVERSED", "ticket-2")));
            assertEquals(1, delivery.reversals(new DeliveryApi.Poll(10)).entries().size());
            delivery.reversalReceipt(new DeliveryApi.ReversalReceipt(order.orderId(), 1, "1001", "REVERSED", PROOF));
            assertEquals("REVERSED", f.store.paymentOrderView(order.orderId()).state());

            // 第二单：偶数修订号(撤销被撤回)人工转 DELIVERED；第一类奇数修订号人工转 REVERSED。
            var order2 = f.store.create("2", new CommerceApi.CreateOrder("manual-rev-002", 1, 1, "buyer-acct-2", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(order2.orderId(), "test", "pay-rev-002", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(order2.orderId(), "1002", "DELIVERED", PROOF));
            revision.record(new CommerceApi.Revision(order2.orderId(), "test", "pay-rev-002", 1, "REVOKED"));
            revision.record(new CommerceApi.Revision(order2.orderId(), "test", "pay-rev-002", 2, "PAID"));
            delivery.reversalReceipt(new DeliveryApi.ReversalReceipt(order2.orderId(), 2, "1002", "MANUAL", PROOF));
            refuses(409, () -> manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-bad-rev-002", order2.orderId(), "MARK_REVERSED", "ticket-3")));
            var delivered = manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-mark-deliv-002", order2.orderId(), "MARK_DELIVERED", "ticket-4"));
            assertEquals("DELIVERED", delivered.resultingState());
            assertEquals("DELIVERED", f.store.paymentOrderView(order2.orderId()).state());
        }
    }

    @Test void 官方交付转人工后可转自动退款或直接标已交付() {
        try (var f = new CommerceFixture()) {
            var manual = manual(f);
            var delivery = delivery(f);
            f.sku(1, 10, 125);
            var o1 = f.store.create("1", new CommerceApi.CreateOrder("manual-deliv-001", 1, 1, "acct-1", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o1.orderId(), "test", "pay-deliv-001", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o1.orderId(), "2001", "MANUAL", PROOF));

            var refundQueued = manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-refund-001", o1.orderId(), "CREATE_REFUND", "ticket-delivery-fail"));
            assertEquals("REFUND_PENDING", refundQueued.resultingState());
            assertEquals("REFUND_PENDING", f.store.paymentOrderView(o1.orderId()).state());
            assertEquals(1, delivery.refunds().size());

            var o2 = f.store.create("2", new CommerceApi.CreateOrder("manual-deliv-002", 1, 1, "acct-2", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o2.orderId(), "test", "pay-deliv-002", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o2.orderId(), "2002", "MANUAL", PROOF));
            var marked = manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("ord-deliv-002", o2.orderId(), "MARK_DELIVERED", "ticket-delivery-ok"));
            assertEquals("DELIVERED", marked.resultingState());
            assertEquals("DELIVERED", f.store.paymentOrderView(o2.orderId()).state());
        }
    }

    @Test void 退款待办转人工后支持重排自动退款_登记线下退款_或标渠道已撤() {
        try (var f = new CommerceFixture()) {
            var manual = manual(f);
            var delivery = delivery(f);
            f.sku(1, 10, 125);

            // 1) 本单退款 MANUAL -> RETRY_REFUND -> 订单 7->5，待办回到可拉取列表
            var o1 = f.store.create("1", new CommerceApi.CreateOrder("manual-ref-001", 1, 1, "acct-r1", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o1.orderId(), "test", "pay-r1", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o1.orderId(), "3001", "REFUND_PENDING", PROOF));
            delivery.closeRefund(new CommerceApi.RefundClosure(o1.orderId(), "test", "pay-r1", "CHANNEL_TEMP_FAIL"));

            // 2) 异常重复付款 MANUAL -> MARK_REFUNDED(带线下流水) -> 只了结待办，不动原订单(DELIVERY_PENDING)
            var o2 = f.store.create("2", new CommerceApi.CreateOrder("manual-ref-002", 1, 1, "acct-r2", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o2.orderId(), "test", "pay-r2-main", 125, "CNY"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o2.orderId(), "test", "pay-r2-dup", 125, "CNY"));
            delivery.closeRefund(new CommerceApi.RefundClosure(o2.orderId(), "test", "pay-r2-dup", "CHANNEL_UNSUPPORTED"));

            // 3) 本单退款 MANUAL -> MARK_REVOKED -> 订单 7->6，不写退款回执
            var o3 = f.store.create("3", new CommerceApi.CreateOrder("manual-ref-003", 1, 1, "acct-r3", "1"));
            f.store.recordTrustedPayment(new CommerceApi.Payment(o3.orderId(), "test", "pay-r3", 125, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o3.orderId(), "3003", "REFUND_PENDING", PROOF));
            delivery.closeRefund(new CommerceApi.RefundClosure(o3.orderId(), "test", "pay-r3", "APPLE_NO_API"));

            var list = manual.list(new ManualAdminApi.ListQuery("", "", "", 20));
            assertEquals(3, list.refunds().size());

            var retry = manual.resolveRefund(ACTOR, new ManualAdminApi.ResolveRefund(
                "ref-retry-001", o1.orderId(), "test", "pay-r1", "RETRY_REFUND", "", "ticket-retry"));
            assertEquals("PENDING", retry.resultingState());
            assertEquals("REFUND_PENDING", f.store.paymentOrderView(o1.orderId()).state());
            assertEquals(1, delivery.refunds().size());

            refuses(400, () -> manual.resolveRefund(ACTOR, new ManualAdminApi.ResolveRefund(
                "ref-missing-ext", o2.orderId(), "test", "pay-r2-dup", "MARK_REFUNDED", "", "ticket-offline")));
            var offline = manual.resolveRefund(ACTOR, new ManualAdminApi.ResolveRefund(
                "ref-offline-002", o2.orderId(), "test", "pay-r2-dup", "MARK_REFUNDED", "offline-ref-888", "ticket-offline"));
            assertEquals("REFUNDED", offline.resultingState());
            assertEquals("DELIVERY_PENDING", f.store.paymentOrderView(o2.orderId()).state(), "异常付款退款不改主订单状态");
            assertEquals(1, f.count("commerce_refund_receipt"));

            var revoked = manual.resolveRefund(ACTOR, new ManualAdminApi.ResolveRefund(
                "ref-revoked-003", o3.orderId(), "test", "pay-r3", "MARK_REVOKED", "", "ticket-apple-self-refund"));
            assertEquals("REVOKED", revoked.resultingState());
            assertEquals("REFUNDED", f.store.paymentOrderView(o3.orderId()).state());
            assertEquals(1, f.count("commerce_refund_receipt"));
        }
    }

    @Test void 寄售人工单支持退还托管资产_解冻卖家收入_以及扣罚争议收入并守住在途提现() {
        try (var f = new CommerceFixture()) {
            var rmbProps = new RmbConsignmentProperties();
            rmbProps.setNoticeMs(0);
            rmbProps.setSaleMs(3_600_000);
            rmbProps.setCoolingMs(60_000);
            rmbProps.setFeeBps(500);
            var rmb = new RmbConsignmentStore(f.jdbc, f.manager, CLOCK, rmbProps,
                (txJdbc, receipt) -> {});
            var orders = new CommerceStore(f.jdbc, f.manager, CLOCK, f.props, rmb);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props, rmb, CLOCK);
            var revision = revision(f);
            var manual = manual(f);

            // 场景 1: 交付前撤款转 MANUAL -> RETURN_TO_SELLER -> 寄售转 RETURNING、订单转 REVERSED(9)
            rmb.register(new RmbConsignmentApi.Escrow("9001", "10", "seller-1", 1, "ITEMS", null, "7001", ESCROW_PROOF, 1000, "玄铁剑"));
            var o1 = orders.createConsignment(new RmbConsignmentApi.Buy("9001", "20", "buyer-1", "buy-c-001", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o1.orderId(), "test", "cpay-001", 1000, "CNY"));
            revision.record(new CommerceApi.Revision(o1.orderId(), "test", "cpay-001", 1, "REVOKED"));
            // 官方订单接口拒绝直接改寄售订单
            refuses(409, () -> manual.resolveOrder(ACTOR,
                new ManualAdminApi.ResolveOrder("wrong-entry-001", o1.orderId(), "MARK_DELIVERED", "ticket")));
            var ret = manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-ret-001", "9001", "RETURN_TO_SELLER", "ticket-revoked-before-delivery"));
            assertEquals("RETURNING", ret.resultingState());
            assertEquals("REVERSED", orders.paymentOrderView(o1.orderId()).state());
            assertEquals(1, rmb.returns(new DeliveryApi.Poll(10)).entries().size());

            // 场景 2: 交付后撤款冻结卖家收入 -> 争议胜诉 UNFREEZE_INCOME -> 立即进入可提现
            rmb.register(new RmbConsignmentApi.Escrow("9002", "11", "seller-2", 1, "ITEMS", null, "7002", ESCROW_PROOF, 1000, "灵玉"));
            var o2 = orders.createConsignment(new RmbConsignmentApi.Buy("9002", "21", "buyer-2", "buy-c-002", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o2.orderId(), "test", "cpay-002", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o2.orderId(), "4002", "DELIVERED", PROOF));
            revision.record(new CommerceApi.Revision(o2.orderId(), "test", "cpay-002", 1, "REVOKED"));
            refuses(409, () -> rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-2", "wd-frozen-001", 950)));
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-bad-ret", "9002", "RETURN_TO_SELLER", "ticket")));
            var unfrozen = manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-unfreeze-002", "9002", "UNFREEZE_INCOME", "ticket-dispute-won"));
            assertEquals("SOLD", unfrozen.resultingState());
            assertEquals("DELIVERED", orders.paymentOrderView(o2.orderId()).state());
            var wd = rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-2", "wd-unfrozen-002", 950));
            assertEquals("REQUESTED", wd.state());

            // 场景 3: 交付后撤款冻结卖家收入 -> 争议败诉 FORFEIT_INCOME -> 计入 forfeited_fen、不可再提现
            rmb.register(new RmbConsignmentApi.Escrow("9003", "12", "seller-3", 1, "ITEMS", null, "7003", ESCROW_PROOF, 1000, "法宝"));
            var o3 = orders.createConsignment(new RmbConsignmentApi.Buy("9003", "22", "buyer-3", "buy-c-003", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o3.orderId(), "test", "cpay-003", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o3.orderId(), "4003", "DELIVERED", PROOF));
            revision.record(new CommerceApi.Revision(o3.orderId(), "test", "cpay-003", 1, "REVOKED"));

            // 模拟有一笔在途提现预留时，必须拒绝扣罚，避免账务冲突。
            f.jdbc.update("UPDATE commerce_seller_income SET reserved_fen=100 WHERE order_sn=?", o3.orderId());
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-forfeit-busy", "9003", "FORFEIT_INCOME", "ticket-busy")));
            f.jdbc.update("UPDATE commerce_seller_income SET reserved_fen=0 WHERE order_sn=?", o3.orderId());

            var forfeited = manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-forfeit-003", "9003", "FORFEIT_INCOME", "ticket-chargeback-upheld"));
            assertEquals("FORFEITED", forfeited.resultingState());
            assertEquals("REVERSED", orders.paymentOrderView(o3.orderId()).state());
            var income3 = rmb.incomes(new RmbConsignmentApi.IncomePage("seller-3", "", 10)).incomes().getFirst();
            assertEquals(950, income3.forfeitedFen());
            assertEquals(Long.MAX_VALUE, income3.availableMs());
            // 即使把 available_ms 误改回过去，forfeited_fen 也保证无法再提现。
            f.jdbc.update("UPDATE commerce_seller_income SET available_ms=? WHERE order_sn=?", CLOCK.millis() - 1, o3.orderId());
            refuses(409, () -> rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-3", "wd-forfeited-003", 1)));
        }
    }

    /** 数据覆写审计 r2-mall#1:付款仍 PAID 时不许退还托管(否则收了钱既不交付也不退款),交付结局只按 trade 的回执落定。 */
    @Test void 寄售交付转人工而付款仍PAID时拒绝退还托管_按交易结局重新收敛交付() {
        try (var f = new CommerceFixture()) {
            var rmbProps = new RmbConsignmentProperties();
            rmbProps.setNoticeMs(0);
            rmbProps.setSaleMs(3_600_000);
            rmbProps.setCoolingMs(60_000);
            rmbProps.setFeeBps(500);
            var rmb = new RmbConsignmentStore(f.jdbc, f.manager, CLOCK, rmbProps, (txJdbc, receipt) -> {});
            var orders = new CommerceStore(f.jdbc, f.manager, CLOCK, f.props, rmb);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props, rmb, CLOCK);
            var revision = revision(f);
            var manual = manual(f);

            // 场景 1: trade 把交付转人工(部分发放 / 合同隔离),付款仍 PAID。退还托管被拒且什么都不改。
            rmb.register(new RmbConsignmentApi.Escrow("9201", "30", "seller-m1", 1, "ITEMS", null, "7201", ESCROW_PROOF, 1000, "玄铁剑"));
            var o1 = orders.createConsignment(new RmbConsignmentApi.Buy("9201", "40", "buyer-m1", "buy-m-001", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o1.orderId(), "test", "mpay-001", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o1.orderId(), "6201", "MANUAL", PROOF));
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-ret-paid-1", "9201", "RETURN_TO_SELLER", "ticket-paid")));
            assertEquals("MANUAL", orders.paymentOrderView(o1.orderId()).state());
            assertEquals("MANUAL", rmb.get(new RmbConsignmentApi.Get("9201")).state());
            assertEquals(0, f.count("commerce_refund_intent"));
            assertTrue(rmb.returns(new DeliveryApi.Poll(10)).entries().isEmpty(), "付款仍 PAID 时不能让 trade 退回托管");

            // RETRY_DELIVERY: 清掉 MANUAL 回执,交付待办回 PENDING、订单回待交付、寄售回 SOLD;同 requestId 重放返回原快照。
            var retryCmd = new ManualAdminApi.ResolveConsignment("consign-retry-m1a", "9201", "RETRY_DELIVERY", "ticket-trade-resumed");
            var retried = manual.resolveConsignment(ACTOR, retryCmd);
            assertEquals("SOLD", retried.resultingState());
            assertEquals(retried, manual.resolveConsignment(ACTOR, retryCmd), "同 requestId 重放返回原提交快照");
            assertEquals("DELIVERY_PENDING", orders.paymentOrderView(o1.orderId()).state());
            assertEquals(0, receipts(f, o1.orderId()));
            assertTrue(pendingDelivery(rmb, o1.orderId()), "trade 能重新拉到这张交付待办");
            // trade 仍在人工:再回 MANUAL,回到人工单;换请求号可以再收敛一次。
            delivery.receipt(new DeliveryApi.Receipt(o1.orderId(), "6201", "MANUAL", PROOF));
            assertEquals("MANUAL", orders.paymentOrderView(o1.orderId()).state());
            manual.resolveConsignment(ACTOR, new ManualAdminApi.ResolveConsignment("consign-retry-m1b", "9201", "RETRY_DELIVERY", "ticket-trade-fixed"));
            // trade 人工处理后交付完成:回执 DELIVERED,同一事务记卖家收入(带冷静期)。
            delivery.receipt(new DeliveryApi.Receipt(o1.orderId(), "6201", "DELIVERED", PROOF));
            assertEquals("DELIVERED", orders.paymentOrderView(o1.orderId()).state());
            var income1 = rmb.incomes(new RmbConsignmentApi.IncomePage("seller-m1", "", 10)).incomes();
            assertEquals(1, income1.size());
            assertEquals(950, income1.getFirst().netFen());
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-retry-m1c", "9201", "RETRY_DELIVERY", "ticket-late")));

            // 场景 2: trade 人工判定不交付:收敛后回执 REFUND_PENDING,建退款待办、寄售转 RETURNING 由 trade 退回托管。
            rmb.register(new RmbConsignmentApi.Escrow("9202", "31", "seller-m2", 1, "ITEMS", null, "7202", ESCROW_PROOF, 1000, "灵玉"));
            var o2 = orders.createConsignment(new RmbConsignmentApi.Buy("9202", "41", "buyer-m2", "buy-m-002", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o2.orderId(), "test", "mpay-002", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o2.orderId(), "6202", "MANUAL", PROOF));
            manual.resolveConsignment(ACTOR, new ManualAdminApi.ResolveConsignment("consign-retry-m2a", "9202", "RETRY_DELIVERY", "ticket-leg-rejected"));
            delivery.receipt(new DeliveryApi.Receipt(o2.orderId(), "6202", "REFUND_PENDING", PROOF));
            assertEquals("REFUND_PENDING", orders.paymentOrderView(o2.orderId()).state());
            assertEquals(1, delivery.refunds().size());
            assertEquals("RETURNING", rmb.get(new RmbConsignmentApi.Get("9202")).state());

            // 场景 3: 交付转人工之后付款被渠道撤销(钱已回到买家):RETRY_DELIVERY 拒绝,RETURN_TO_SELLER 放行。
            rmb.register(new RmbConsignmentApi.Escrow("9203", "32", "seller-m3", 1, "ITEMS", null, "7203", ESCROW_PROOF, 1000, "法宝"));
            var o3 = orders.createConsignment(new RmbConsignmentApi.Buy("9203", "42", "buyer-m3", "buy-m-003", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o3.orderId(), "test", "mpay-003", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o3.orderId(), "6203", "MANUAL", PROOF));
            revision.record(new CommerceApi.Revision(o3.orderId(), "test", "mpay-003", 1, "REVOKED"));
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-retry-m3", "9203", "RETRY_DELIVERY", "ticket-revoked")));
            var returned = manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-ret-m3", "9203", "RETURN_TO_SELLER", "ticket-revoked"));
            assertEquals("RETURNING", returned.resultingState());
            assertEquals("REVERSED", orders.paymentOrderView(o3.orderId()).state());

            // 场景 4: 交付前撤款被商城拦下(交付待办 REVOKED),之后撤款又被撤回(付款回到 PAID):只能重新收敛交付。
            rmb.register(new RmbConsignmentApi.Escrow("9204", "33", "seller-m4", 1, "ITEMS", null, "7204", ESCROW_PROOF, 1000, "宝珠"));
            var o4 = orders.createConsignment(new RmbConsignmentApi.Buy("9204", "43", "buyer-m4", "buy-m-004", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o4.orderId(), "test", "mpay-004", 1000, "CNY"));
            revision.record(new CommerceApi.Revision(o4.orderId(), "test", "mpay-004", 1, "REVOKED"));
            revision.record(new CommerceApi.Revision(o4.orderId(), "test", "mpay-004", 2, "PAID"));
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-ret-paid-4", "9204", "RETURN_TO_SELLER", "ticket-reinstated")));
            assertEquals("SOLD", manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-retry-m4", "9204", "RETRY_DELIVERY", "ticket-reinstated")).resultingState());
            assertTrue(pendingDelivery(rmb, o4.orderId()));
            delivery.receipt(new DeliveryApi.Receipt(o4.orderId(), "6204", "DELIVERED", PROOF));
            assertEquals("DELIVERED", orders.paymentOrderView(o4.orderId()).state());

            // 场景 5: 已交付、撤款冻结收入之后撤款又被撤回:交付已有结局,不能重开交付(收入走 UNFREEZE_INCOME / FORFEIT_INCOME)。
            rmb.register(new RmbConsignmentApi.Escrow("9205", "34", "seller-m5", 1, "ITEMS", null, "7205", ESCROW_PROOF, 1000, "灵符"));
            var o5 = orders.createConsignment(new RmbConsignmentApi.Buy("9205", "44", "buyer-m5", "buy-m-005", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(o5.orderId(), "test", "mpay-005", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(o5.orderId(), "6205", "DELIVERED", PROOF));
            revision.record(new CommerceApi.Revision(o5.orderId(), "test", "mpay-005", 1, "REVOKED"));
            revision.record(new CommerceApi.Revision(o5.orderId(), "test", "mpay-005", 2, "PAID"));
            refuses(409, () -> manual.resolveConsignment(ACTOR,
                new ManualAdminApi.ResolveConsignment("consign-retry-m5", "9205", "RETRY_DELIVERY", "ticket-delivered")));
            assertEquals("MANUAL", orders.paymentOrderView(o5.orderId()).state());
        }
    }

    @Test void G2_逐单争议冻结与提现同账号锁拦截_支持部分冻结与释放审计() {
        try (var f = new CommerceFixture()) {
            var manual = manual(f);
            var props = new RmbConsignmentProperties();
            props.setNoticeMs(0);
            props.setCoolingMs(0);
            props.validate();
            var rmb = new RmbConsignmentStore(f.jdbc, f.manager, CLOCK, props, (RmbConsignmentStore.PayoutEvidenceVerifier) null);
            var orders = new CommerceStore(f.jdbc, f.manager, CLOCK, f.props, rmb);
            var delivery = new DeliveryStore(f.jdbc, f.manager, f.props, rmb);

            // 生成一笔净额 950 分的卖家收益
            rmb.register(new RmbConsignmentApi.Escrow("9101", "15", "seller-dispute", 1, "ITEMS", null, "trade:9101", ESCROW_PROOF, 1000, "托管材料"));
            var order = orders.createConsignment(new RmbConsignmentApi.Buy("9101", "25", "buyer-dispute", "buy-dispute-001", "1"));
            orders.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "cpay-dispute-1", 1000, "CNY"));
            delivery.receipt(new DeliveryApi.Receipt(order.orderId(), "5101", "DELIVERED", PROOF));

            // 争议冻结 400 分（幂等重放不重复加冻）
            var freezeCmd = new ManualAdminApi.ResolveIncomeDispute("dispute-frz-001", order.orderId(), "FREEZE_DISPUTE", 400, "CASE-2026-001", "buyer complaint");
            var frozen = manual.resolveIncomeDispute(ACTOR, freezeCmd);
            assertEquals("DISPUTE_FROZEN:400", frozen.resultingState());
            assertEquals(frozen, manual.resolveIncomeDispute(ACTOR, freezeCmd));
            var incomeAfterFreeze = rmb.incomes(new RmbConsignmentApi.IncomePage("seller-dispute", "", 10)).incomes().getFirst();
            assertEquals(400, incomeAfterFreeze.disputeFrozenFen());
            assertEquals("CASE-2026-001", incomeAfterFreeze.disputeCaseId());

            // 此时仅剩 550 分可提现：提 551 分被拒，提 550 分成功
            refuses(409, () -> rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-dispute", "wd-too-much-001", 551)));
            var wd1 = rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-dispute", "wd-ok-550", 550));
            assertEquals(550, wd1.amountFen());

            // 再尝试冻结超出剩余自由额的资金必须 fail-closed 拒绝（不能假装冻结在途提现款）
            refuses(409, () -> manual.resolveIncomeDispute(ACTOR,
                new ManualAdminApi.ResolveIncomeDispute("dispute-frz-002", order.orderId(), "FREEZE_DISPUTE", 1, "CASE-2026-002", "over-freeze")));

            // 争议释放 400 分后，dispute_case_id 清空，剩余 400 分恢复可提现
            var released = manual.resolveIncomeDispute(ACTOR,
                new ManualAdminApi.ResolveIncomeDispute("dispute-rel-001", order.orderId(), "RELEASE_DISPUTE", 400, "CASE-2026-001", "dispute rejected"));
            assertEquals("DISPUTE_RELEASED", released.resultingState());
            var incomeAfterRelease = rmb.incomes(new RmbConsignmentApi.IncomePage("seller-dispute", "", 10)).incomes().getFirst();
            assertEquals(0, incomeAfterRelease.disputeFrozenFen());
            assertNull(incomeAfterRelease.disputeCaseId());
            var wd2 = rmb.withdraw(new RmbConsignmentApi.Withdraw("seller-dispute", "wd-ok-400", 400));
            assertEquals(400, wd2.amountFen());
        }
    }

    private static ManualAdminStore manual(CommerceFixture f) {
        return new ManualAdminStore(f.jdbc, f.manager, new ObjectMapper(), CLOCK);
    }

    private static DeliveryStore delivery(CommerceFixture f) {
        return new DeliveryStore(f.jdbc, f.manager, f.props);
    }

    private static PaymentRevisionStore revision(CommerceFixture f) {
        return new PaymentRevisionStore(f.jdbc, f.manager, CLOCK, f.props);
    }

    private static int receipts(CommerceFixture f, String orderId) {
        return f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_delivery_receipt WHERE order_sn=?", Integer.class, orderId);
    }

    private static boolean pendingDelivery(RmbConsignmentStore rmb, String orderId) {
        return rmb.deliveries(new DeliveryApi.Poll(100)).entries().stream().anyMatch(e -> e.intent().orderId().equals(orderId));
    }

    private static void refuses(int status, Executable call) {
        assertEquals(status, assertThrows(CommerceException.class, call).status());
    }
}
