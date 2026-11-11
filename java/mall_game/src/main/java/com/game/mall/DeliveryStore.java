package com.game.mall;

import java.nio.charset.StandardCharsets;
import java.util.*;
import java.time.Clock;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

/** Mall侧可靠交付意图；相同order只能绑定一个trade合同，未知结果保留原意图。 */
@Component
public final class DeliveryStore {
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final PaymentPolicy paymentPolicy;
    private final RmbConsignmentStore consignments;
    public DeliveryStore(JdbcTemplate jdbc, PlatformTransactionManager manager, CommerceProperties props) {
        this(jdbc, manager, props, new RmbConsignmentStore(jdbc, manager, Clock.systemUTC(),
            new RmbConsignmentProperties(), (RmbConsignmentStore.PayoutEvidenceVerifier) null));
    }
    @Autowired public DeliveryStore(JdbcTemplate jdbc, PlatformTransactionManager manager, CommerceProperties props,
            RmbConsignmentStore consignments) {
        this.jdbc = jdbc;
        this.consignments = consignments;
        paymentPolicy = new PaymentPolicy(jdbc, props);
        tx = new TransactionTemplate(manager);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setTimeout(5);
    }
    public DeliveryApi.Skus skus() {
        return skus(new DeliveryApi.SkuPage(0, 100));
    }
    public DeliveryApi.Skus skus(DeliveryApi.SkuPage page) {
        if (page == null || page.afterSkuId() < 0 || page.limit() < 1 || page.limit() > 100)
            throw new CommerceException(400, "商品分页非法");
        return new DeliveryApi.Skus(jdbc.query("SELECT s.id,g.title,s.price_minor,s.currency,s.stock-s.lock_stock FROM "
                + "pms_sku_stock s JOIN commerce_sku_grant g ON g.sku_id=s.id WHERE "
                + "s.published=1 AND s.stock>s.lock_stock AND s.id>? ORDER BY s.id LIMIT ?",
            (r, n)
                -> new DeliveryApi.Sku(r.getLong(1), r.getString(2), r.getLong(3), r.getString(4), r.getLong(5)),
            page.afterSkuId(), page.limit()));
    }
    public List<DeliveryApi.Intent> poll(int limit) {
        return poll(new DeliveryApi.Poll(limit)).entries().stream().map(DeliveryApi.PollEntry::intent).toList();
    }
    private record PendingRow(DeliveryApi.Intent intent, long createdMs) {}
    public DeliveryApi.PollPage poll(DeliveryApi.Poll input) {
        if (input == null || input.limit() < 1 || input.limit() > 100)
            throw new CommerceException(400, "交付批量非法");
        PendingScanCursor cursor = PendingScanCursor.parse(input.cursor());
        if (cursor == null) {
            var upper = jdbc.query("SELECT created_ms,order_sn FROM commerce_delivery_intent WHERE status='PENDING' "
                    + "ORDER BY created_ms DESC,order_sn DESC LIMIT 1",
                (r, n) -> new PendingScanCursor(0, "", r.getLong(1), r.getString(2)));
            if (upper.isEmpty())
                return new DeliveryApi.PollPage(List.of());
            cursor = upper.getFirst();
        }
        // 固定本轮上界；新单留给下轮，持续新增也不能让离线老单永远等不到绕回。
        var rows = jdbc.query("SELECT "
                          + "d.order_sn,d.buyer_id,d.sku_id,d.quantity,g.item_config_id,g.count_per_unit,g.title,o.pay_"
                          + "amount,o.currency,d.created_ms,p.buyer_account,p.buyer_owner_revision,o.buyer_id,o.sku_id,o.quantity "
                + "FROM commerce_delivery_intent d JOIN commerce_order_grant g ON g.order_sn=d.order_sn JOIN oms_order "
                  + "o ON o.order_sn=d.order_sn LEFT JOIN commerce_official_owner p ON p.order_sn=d.order_sn "
                + "WHERE d.status='PENDING' AND d.sku_id<>0 AND (d.created_ms>? OR (d.created_ms=? AND d.order_sn>?)) "
                + "AND (d.created_ms<? OR (d.created_ms=? AND d.order_sn<=?)) "
                + "ORDER BY d.created_ms,d.order_sn LIMIT ?",
            (r, n)
                -> {
                long count = Math.multiplyExact(r.getInt(4), (long) r.getInt(6));
                if (count < 1 || count > 4096)
                    throw new CommerceException(503, "已冻结的SKU游戏数量越界");
                if (!r.getString(2).equals(r.getString(13)) || r.getLong(3) != r.getLong(14) || r.getInt(4) != r.getInt(15))
                    throw new CommerceException(503, "交付意图与原OMS角色或商品不符");
                return new PendingRow(new DeliveryApi.Intent(r.getString(1), r.getString(2),
                    CommerceIdentity.storedAccount(r.getBytes(11)), CommerceIdentity.storedRevision(r.getBytes(12)),
                    r.getLong(3), r.getInt(4), r.getInt(5),
                    (int) count, r.getString(7), r.getLong(8), r.getString(9)), r.getLong(10));
            },
            cursor.afterMs(), cursor.afterMs(), cursor.afterId(), cursor.upperMs(), cursor.upperMs(), cursor.upperId(), input.limit() + 1);
        boolean hasMore = rows.size() > input.limit();
        int count = Math.min(rows.size(), input.limit());
        List<DeliveryApi.PollEntry> entries = new ArrayList<>();
        for (int i = 0; i < count; i++) {
            var row = rows.get(i);
            entries.add(new DeliveryApi.PollEntry(row.intent(), !hasMore && i == count - 1 ? ""
                : cursor.after(row.createdMs(), row.intent().orderId())));
        }
        return new DeliveryApi.PollPage(List.copyOf(entries));
    }
    public void receipt(DeliveryApi.Receipt in) {
        if (in == null || in.orderId() == null || !in.orderId().matches("[0-9a-f-]{36}") || in.tradeOrderId() == null
            || !in.tradeOrderId().matches("[1-9][0-9]{0,19}") || in.proof() == null
            || !in.proof().matches("[0-9a-f]{64}")
            || !Set.of("DELIVERED", "REFUND_PENDING", "MANUAL").contains(in.state()))
            throw new CommerceException(400, "交付持久回执非法");
        tx.executeWithoutResult(status -> {
            // 先锁原OMS，与付款/取消同序；收入和可信资产终局在同一个事务提交。
            var orders = jdbc.queryForList("SELECT status,sku_id FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            var rows = jdbc.queryForList(
                "SELECT status FROM commerce_delivery_intent WHERE order_sn=? FOR UPDATE", in.orderId());
            if (rows.size() != 1)
                throw new CommerceException(404, "交付意图不存在");
            var existing = jdbc.queryForList(
                "SELECT trade_order_id,state,proof FROM commerce_delivery_receipt WHERE order_sn=?", in.orderId());
            if (!existing.isEmpty()) {
                var old = existing.getFirst();
                if (!in.tradeOrderId().equals(old.get("trade_order_id")) || !in.state().equals(old.get("state"))
                    || !in.proof().equals(old.get("proof")))
                    throw new CommerceException(409, "同一交付意图的终局发生冲突");
                return;
            }
            if (!"PENDING".equals(rows.getFirst().get("status"))) {
                throw new CommerceException(409, "交付意图已终结但缺原始回执");
            }
            if (orders.size() != 1 || ((Number) orders.getFirst().get("status")).intValue() != 1) {
                throw new CommerceException(409, "交付订单不在已支付待交付状态");
            }
            if (((Number) orders.getFirst().get("sku_id")).longValue() == 0) {
                if (consignments.mapping(in.orderId()) == null) { throw new CommerceException(503, "寄售交付原身份缺失"); }
            } else {
                var original = jdbc.query("SELECT buyer_account,buyer_owner_revision FROM commerce_official_owner WHERE order_sn=?",
                    (row,index)->List.of(CommerceIdentity.storedAccount(row.getBytes(1)),CommerceIdentity.storedRevision(row.getBytes(2))),in.orderId());
                if (original.size() != 1) { throw new CommerceException(503, "官方交付原身份缺失"); }
            }
            consignments.delivered(in.orderId(), in.state());
            jdbc.update("INSERT INTO commerce_delivery_receipt(order_sn,trade_order_id,state,proof) VALUES(?,?,?,?)",
                in.orderId(), in.tradeOrderId(), in.state(), in.proof());
            jdbc.update("UPDATE commerce_delivery_intent SET status=? WHERE order_sn=? AND status='PENDING'",
                in.state(), in.orderId());
            if (in.state().equals("DELIVERED"))
                jdbc.update("UPDATE oms_order SET status=2 WHERE order_sn=? AND status=1", in.orderId());
            if (in.state().equals("MANUAL"))
                jdbc.update("UPDATE oms_order SET status=7 WHERE order_sn=? AND status=1", in.orderId());
            if (in.state().equals("REFUND_PENDING")) {
                // 交付被拒:退的是订单绑定的那笔付款,退款待办按它的交易键建(一张订单可能另有异常付款的待办)。
                int intents = jdbc.update("INSERT INTO commerce_refund_intent(channel,transaction_id,order_sn,source,status) "
                    + "SELECT channel,transaction_id,order_sn,'" + CommerceApi.RefundReason.DELIVERY_REJECTED.name() + "','PENDING' "
                    + "FROM commerce_payment_receipt WHERE order_sn=?", in.orderId());
                if (intents != 1) { throw new CommerceException(503, "交付退款缺订单付款回执"); }
                jdbc.update("UPDATE oms_order SET status=5 WHERE order_sn=? AND status=1", in.orderId());
            }
        });
    }

    /**
     * 待退款的付款,每条是要退的那一笔(退款原因见 CommerceApi.RefundReason):交易键取待办自己的,金额币种取那笔付款回执的实收值。
     * 异常付款的回执没有绑定订单(order_sn 为 NULL),订单号取待办上的。适配器以 transactionId 作渠道退款的幂等键(CommerceApi.PendingRefund)。
     */
    public List<CommerceApi.PendingRefund> refunds() {
        return jdbc.query("SELECT r.order_sn,p.channel,p.transaction_id,p.amount_minor,p.currency,r.source FROM "
                + "commerce_refund_intent r JOIN commerce_payment_receipt p ON p.channel=r.channel AND p.transaction_id=r.transaction_id "
                + "WHERE r.status='PENDING' ORDER BY r.order_sn,r.channel,r.transaction_id LIMIT 100",
            (r, n)
                -> new CommerceApi.PendingRefund(r.getString(1), r.getString(2), r.getString(3), r.getLong(4), r.getString(5),
                    CommerceApi.RefundReason.stored(r.getString(6))));
    }

    /**
     * 渠道报告某一笔付款已退款。按被退付款的交易键 (channel, paymentTransactionId) 定位退款待办并幂等:
     * 同一笔付款只认一条退款流水,重放同一条回 200,换一条回 409;一条退款流水已了结别的付款也回 409。
     * 只有订单本身的退款(CANCELLED_LATE / DELIVERY_REJECTED)把订单 5→6;异常付款(重复付款、金额或币种不符)的退款
     * 只终结它自己的待办,订单和订单绑定的那笔付款都不动。
     */
    public void refunded(CommerceApi.Refund in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.channel() == null || !in.channel().matches("[a-z][a-z0-9_-]{1,31}")
            || in.paymentTransactionId() == null || !in.paymentTransactionId().matches("[A-Za-z0-9_-]{1,128}")
            || in.transactionId() == null || !in.transactionId().matches("[A-Za-z0-9_-]{1,128}"))
            throw new CommerceException(400, "退款回执非法");
        try {
            settleRefund(in);
        } catch (DuplicateKeyException reused) {
            // 退款流水 (channel, transaction_id) 唯一:渠道的一条退款只能了结一笔付款。不映射的话会以 500 冒出,适配器会一直重试。
            throw new CommerceException(409, "退款流水已用于其他付款");
        }
    }

    private void settleRefund(CommerceApi.Refund in) {
        tx.executeWithoutResult(status -> {
            // 锁序与付款、取消、交付回执相同:先单表按唯一键锁原 OMS 订单行(唯一键索引→订单记录,与mock原件同序;
            // JOIN可能先锁记录再等索引而成环),再按主键锁这笔付款的退款待办。
            var lockedOrder = jdbc.queryForList("SELECT status FROM oms_order WHERE order_sn=? FOR UPDATE",
                in.orderId());
            if (lockedOrder.size() != 1)
                throw new CommerceException(404, "退款订单不存在");
            var intents = jdbc.queryForList("SELECT order_sn,source,status FROM commerce_refund_intent "
                + "WHERE channel=? AND transaction_id=? FOR UPDATE", ascii(in.channel()), ascii(in.paymentTransactionId()));
            if (intents.size() != 1 || !in.orderId().equals(intents.getFirst().get("order_sn")))
                throw new CommerceException(404, "退款待办不存在");
            var intent = intents.getFirst();
            var original = jdbc.queryForList("SELECT order_sn,amount_minor,currency FROM commerce_payment_receipt "
                + "WHERE channel=? AND transaction_id=?", ascii(in.channel()), ascii(in.paymentTransactionId()));
            if (original.size() != 1)
                throw new CommerceException(503, "退款待办缺原付款回执");
            var row = original.getFirst();
            if (((Number) row.get("amount_minor")).longValue() != in.amountMinor()
                || !row.get("currency").equals(in.currency()))
                throw new CommerceException(409, "退款金额币种不符");
            // 订单本身的退款 ⇔ 退的是订单绑定的那笔;异常付款从不绑定订单。两者对不上说明存储已损坏,拒绝而不是猜。
            boolean ordersOwnPayment = CommerceApi.RefundReason.stored((String) intent.get("source")).refundsOrdersOwnPayment();
            if (ordersOwnPayment != in.orderId().equals(row.get("order_sn")))
                throw new CommerceException(503, "退款待办来源与付款绑定不一致");
            paymentPolicy.refund(in);
            var prior = jdbc.queryForList("SELECT transaction_id FROM commerce_refund_receipt "
                + "WHERE channel=? AND payment_transaction_id=?", String.class, ascii(in.channel()), ascii(in.paymentTransactionId()));
            if (!prior.isEmpty()) {
                if (!in.transactionId().equals(prior.getFirst()))
                    throw new CommerceException(409, "退款流水冲突");
                return;
            }
            if (!"PENDING".equals(intent.get("status")))
                throw new CommerceException(503, "退款待办已终结但缺退款回执");
            if (ordersOwnPayment && ((Number) lockedOrder.getFirst().get("status")).intValue() != 5)
                throw new CommerceException(409, "订单不在退款中");
            jdbc.update("INSERT INTO commerce_refund_receipt(channel,payment_transaction_id,transaction_id,order_sn,amount_minor,currency) "
                    + "VALUES(?,?,?,?,?,?)",
                ascii(in.channel()), ascii(in.paymentTransactionId()), ascii(in.transactionId()), in.orderId(),
                in.amountMinor(), in.currency());
            if (jdbc.update("UPDATE commerce_refund_intent SET status='REFUNDED' WHERE channel=? AND transaction_id=? AND status='PENDING'",
                    ascii(in.channel()), ascii(in.paymentTransactionId())) != 1)
                throw new CommerceException(409, "退款待办状态冲突");
            if (ordersOwnPayment && jdbc.update("UPDATE oms_order SET status=6 WHERE order_sn=? AND status=5", in.orderId()) != 1)
                throw new CommerceException(409, "订单状态冲突");
        });
    }

    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
