package com.game.mall;

import java.nio.charset.StandardCharsets;
import java.sql.ResultSet;
import java.sql.SQLException;
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
    /** 一单货币发放的上限,与商城金额上限同一量级;下单冻结时(CommerceStore.create)按同一个值拦。 */
    static final long MAX_CURRENCY_GRANT = 9_000_000_000_000_000L;
    /**
     * 交付待办与扣回待办共用的订单条款列(Intent 的来源)。同一张订单走这两条路给 trade 的 Intent 必须逐字段相同:
     * trade 按 Intent 的摘要找回同一张合同(go/trade/internal/commerce/mall.go 的 accept)。调用方以别名 d 引入交付待办。
     */
    private static final String INTENT_COLUMNS = "d.order_sn,d.buyer_id,d.sku_id,d.quantity,g.item_config_id,g.count_per_unit,g.title,"
        + "o.pay_amount,o.currency,p.buyer_account,p.buyer_owner_revision,o.buyer_id,o.sku_id,o.quantity,g.currency_type,g.currency_amount";
    private static final String INTENT_JOINS = "JOIN commerce_order_grant g ON g.order_sn=d.order_sn JOIN oms_order o ON o.order_sn=d.order_sn "
        + "LEFT JOIN commerce_official_owner p ON p.order_sn=d.order_sn";
    /** INTENT_COLUMNS 之后的第一列。 */
    private static final int AFTER_INTENT = 17;
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final PaymentPolicy paymentPolicy;
    private final CommerceProperties props;
    private final RmbConsignmentStore consignments;
    private final Clock clock;
    public DeliveryStore(JdbcTemplate jdbc, PlatformTransactionManager manager, CommerceProperties props) {
        this(jdbc, manager, props, new RmbConsignmentStore(jdbc, manager, Clock.systemUTC(),
            new RmbConsignmentProperties(), (RmbConsignmentStore.PayoutEvidenceVerifier) null), Clock.systemUTC());
    }
    @Autowired public DeliveryStore(JdbcTemplate jdbc, PlatformTransactionManager manager, CommerceProperties props,
            RmbConsignmentStore consignments, Clock clock) {
        this.jdbc = jdbc;
        this.consignments = consignments;
        this.props = props;
        this.clock = clock;
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
    private record PendingRow(DeliveryApi.Intent intent, long createdMs, int paymentRevision) {}
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
        // paymentRevision:这张订单绑定付款的最新修订号(有扣回待办时),没有为 0。trade 拿它决定先发放还是记作废标记,
        // 不会因为交付待办先于扣回待办被看到而把已撤销的订单先发出去再扣回来。
        var rows = jdbc.query("SELECT " + INTENT_COLUMNS + ",d.created_ms,COALESCE(v.revision,0) FROM commerce_delivery_intent d " + INTENT_JOINS
                + " LEFT JOIN commerce_reversal_intent v ON v.order_sn=d.order_sn"
                + " WHERE d.status='PENDING' AND d.sku_id<>0 AND (d.created_ms>? OR (d.created_ms=? AND d.order_sn>?)) "
                + "AND (d.created_ms<? OR (d.created_ms=? AND d.order_sn<=?)) "
                + "ORDER BY d.created_ms,d.order_sn LIMIT ?",
            (r, n) -> new PendingRow(intent(r), r.getLong(AFTER_INTENT), r.getInt(AFTER_INTENT + 1)),
            cursor.afterMs(), cursor.afterMs(), cursor.afterId(), cursor.upperMs(), cursor.upperMs(), cursor.upperId(), input.limit() + 1);
        boolean hasMore = rows.size() > input.limit();
        int count = Math.min(rows.size(), input.limit());
        List<DeliveryApi.PollEntry> entries = new ArrayList<>();
        for (int i = 0; i < count; i++) {
            var row = rows.get(i);
            entries.add(new DeliveryApi.PollEntry(row.intent(), !hasMore && i == count - 1 ? ""
                : cursor.after(row.createdMs(), row.intent().orderId()), row.paymentRevision()));
        }
        return new DeliveryApi.PollPage(List.copyOf(entries));
    }

    /**
     * 一行订单条款(INTENT_COLUMNS)转成 Intent。发放物二选一:物品(itemConfigId × itemCount,已乘购买数量,一单至多 4096 件)
     * 或货币(currencyType × currencyAmount,已乘购买数量),另一种全为 0。货币种类 0 是银两,有没有货币发放看数量是否大于 0。
     * 冻结的条款越界或两种都有说明存储已坏,回 503。
     */
    private static DeliveryApi.Intent intent(ResultSet r) throws SQLException {
        if (!r.getString(2).equals(r.getString(12)) || r.getLong(3) != r.getLong(13) || r.getInt(4) != r.getInt(14))
            throw new CommerceException(503, "交付意图与原OMS角色或商品不符");
        int quantity = r.getInt(4), itemConfigId = r.getInt(5), perUnit = r.getInt(6), currencyType = r.getInt(15);
        long currencyPerUnit = r.getLong(16);
        int itemCount = 0;
        long currencyAmount = 0;
        try {
            if (itemConfigId > 0 && perUnit > 0 && currencyType == 0 && currencyPerUnit == 0) {
                long count = Math.multiplyExact(quantity, (long) perUnit);
                if (count < 1 || count > 4096)
                    throw new CommerceException(503, "已冻结的SKU游戏数量越界");
                itemCount = (int) count;
            } else if (itemConfigId == 0 && perUnit == 0 && currencyType >= 0 && currencyPerUnit > 0 && quantity > 0) {
                currencyAmount = Math.multiplyExact(quantity, currencyPerUnit);
                if (currencyAmount > MAX_CURRENCY_GRANT)
                    throw new CommerceException(503, "已冻结的SKU货币数量越界");
            } else {
                throw new CommerceException(503, "已冻结的SKU发放物非法");
            }
        } catch (ArithmeticException overflow) {
            throw new CommerceException(503, "已冻结的SKU发放数量溢出");
        }
        return new DeliveryApi.Intent(r.getString(1), r.getString(2),
            CommerceIdentity.storedAccount(r.getBytes(10)), CommerceIdentity.storedRevision(r.getBytes(11)),
            r.getLong(3), quantity, itemConfigId, itemCount, r.getString(7), r.getLong(8), r.getString(9), currencyType, currencyAmount);
    }

    /**
     * 交付回执。state:DELIVERED 已发放、REFUND_PENDING 拒收转退款、MANUAL 转人工、VOIDED 没有发放(付款在交付之前已被渠道撤销,
     * trade 记了作废标记,见扣回待办)。
     * <p>订单有扣回待办(commerce_reversal_intent 有行)时,订单状态归扣回流程管(PaymentRevisionStore、reversalReceipt):
     * 这张回执只了结交付待办、留下原始回执,不改订单、不建退款待办 —— 付款已被撤销,或撤销又被撤回、该不该持有由扣回回执定。
     * 没有扣回待办时与之前相同:订单必须在已支付待交付(1),VOIDED 不成立。
     */
    public void receipt(DeliveryApi.Receipt in) {
        if (in == null || in.orderId() == null || !in.orderId().matches("[0-9a-f-]{36}") || in.tradeOrderId() == null
            || !in.tradeOrderId().matches("[1-9][0-9]{0,19}") || in.proof() == null
            || !in.proof().matches("[0-9a-f]{64}")
            || !Set.of("DELIVERED", "REFUND_PENDING", "MANUAL", "VOIDED").contains(in.state()))
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
            if (orders.size() != 1)
                throw new CommerceException(409, "交付订单不在已支付待交付状态");
            // 锁序:订单 → 交付待办 → 扣回待办;PaymentRevisionStore 写扣回待办时锁订单之后不碰交付待办,不成环。
            boolean reversalOwned = !jdbc.queryForList("SELECT revision FROM commerce_reversal_intent WHERE order_sn=? FOR UPDATE",
                in.orderId()).isEmpty();
            if (!reversalOwned && (((Number) orders.getFirst().get("status")).intValue() != 1 || in.state().equals("VOIDED"))) {
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
            if (reversalOwned)
                return;
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

    /**
     * 退款转人工(payment 角色,/internal/refunds/close):这笔付款的渠道退款做不了 —— 渠道没有开发者退款接口(App Store),
     * 或渠道明确退款失败。退款待办 PENDING → MANUAL 并记原因,不再列在待退款里(待退款一页只取 100 条,退不了的不能一直占着);
     * 订单本身那笔(取消后迟到、交付被拒)转人工时订单 5 → 7。已是 MANUAL 的重放回 200;已退成或已被渠道撤销回 409。
     */
    public void closeRefund(CommerceApi.RefundClosure in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.channel() == null || !in.channel().matches("[a-z][a-z0-9_-]{1,31}")
            || in.paymentTransactionId() == null || !in.paymentTransactionId().matches("[A-Za-z0-9_-]{1,128}")
            || in.reason() == null || !in.reason().matches("[A-Za-z0-9_.:-]{1,64}"))
            throw new CommerceException(400, "退款转人工参数非法");
        props.requirePaymentChannel(in.channel());
        tx.executeWithoutResult(status -> {
            // 锁序与退款回执相同:先订单行,再这笔付款的退款待办。
            var lockedOrder = jdbc.queryForList("SELECT status FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (lockedOrder.size() != 1)
                throw new CommerceException(404, "退款订单不存在");
            var intents = jdbc.queryForList("SELECT order_sn,source,status FROM commerce_refund_intent "
                + "WHERE channel=? AND transaction_id=? FOR UPDATE", ascii(in.channel()), ascii(in.paymentTransactionId()));
            if (intents.size() != 1 || !in.orderId().equals(intents.getFirst().get("order_sn")))
                throw new CommerceException(404, "退款待办不存在");
            String current = (String) intents.getFirst().get("status");
            if ("MANUAL".equals(current))
                return;
            if (!"PENDING".equals(current))
                throw new CommerceException(409, "退款待办已终结");
            jdbc.update("UPDATE commerce_refund_intent SET status='MANUAL',close_reason=? WHERE channel=? AND transaction_id=? AND status='PENDING'",
                in.reason(), ascii(in.channel()), ascii(in.paymentTransactionId()));
            boolean ordersOwnPayment = CommerceApi.RefundReason.stored((String) intents.getFirst().get("source")).refundsOrdersOwnPayment();
            if (ordersOwnPayment && jdbc.update("UPDATE oms_order SET status=7 WHERE order_sn=? AND status=5", in.orderId()) != 1)
                throw new CommerceException(503, "订单本身的退款待办与订单状态不一致");
        });
    }

    /**
     * 扣回待办(delivery 角色,/internal/reversals/poll):订单绑定的付款被渠道撤销或撤销又被撤回之后,trade 要把这张订单的发放
     * 收敛到最新修订号(PaymentRevisionStore 写)。翻页与交付待办同一种游标(PendingScanCursor,按改写时间与订单号),
     * 每轮固定上界;intent 与交付待办逐字段相同,entitled 由修订号奇偶得出(偶数 = 该持有)。
     */
    public DeliveryApi.ReversalPage reversals(DeliveryApi.Poll input) {
        if (input == null || input.limit() < 1 || input.limit() > 100)
            throw new CommerceException(400, "扣回批量非法");
        PendingScanCursor cursor = PendingScanCursor.parse(input.cursor());
        if (cursor == null) {
            var upper = jdbc.query("SELECT updated_ms,order_sn FROM commerce_reversal_intent WHERE status='PENDING' "
                    + "ORDER BY updated_ms DESC,order_sn DESC LIMIT 1",
                (r, n) -> new PendingScanCursor(0, "", r.getLong(1), r.getString(2)));
            if (upper.isEmpty())
                return new DeliveryApi.ReversalPage(List.of());
            cursor = upper.getFirst();
        }
        record Row(DeliveryApi.Intent intent, long updatedMs, int revision) {}
        var rows = jdbc.query("SELECT " + INTENT_COLUMNS + ",v.updated_ms,v.revision FROM commerce_reversal_intent v "
                + "JOIN commerce_delivery_intent d ON d.order_sn=v.order_sn " + INTENT_JOINS
                + " WHERE v.status='PENDING' AND (v.updated_ms>? OR (v.updated_ms=? AND v.order_sn>?)) "
                + "AND (v.updated_ms<? OR (v.updated_ms=? AND v.order_sn<=?)) "
                + "ORDER BY v.updated_ms,v.order_sn LIMIT ?",
            (r, n) -> new Row(intent(r), r.getLong(AFTER_INTENT), r.getInt(AFTER_INTENT + 1)),
            cursor.afterMs(), cursor.afterMs(), cursor.afterId(), cursor.upperMs(), cursor.upperMs(), cursor.upperId(), input.limit() + 1);
        boolean hasMore = rows.size() > input.limit();
        int count = Math.min(rows.size(), input.limit());
        List<DeliveryApi.ReversalEntry> entries = new ArrayList<>();
        for (int i = 0; i < count; i++) {
            var row = rows.get(i);
            if (row.revision() < 1)
                throw new CommerceException(503, "扣回待办修订号非法");
            entries.add(new DeliveryApi.ReversalEntry(row.intent(), row.revision(), row.revision() % 2 == 0,
                !hasMore && i == count - 1 ? "" : cursor.after(row.updatedMs(), row.intent().orderId())));
        }
        return new DeliveryApi.ReversalPage(List.copyOf(entries));
    }

    /**
     * 扣回回执(delivery 角色,/internal/reversals/receipt):trade 已把这张订单的发放收敛到 revision。按 (订单, 修订号) 幂等,
     * 同一修订号换一种结论回 409。只有最新修订号的回执改待办与订单:REVERSED → 9、DELIVERED → 2、MANUAL → 7;
     * 旧修订号的回执(之后又来了新修订号)只留档。结论必须与修订号相符:撤销(奇数)只能 REVERSED / MANUAL,
     * 恢复(偶数)只能 DELIVERED / MANUAL。
     */
    public void reversalReceipt(DeliveryApi.ReversalReceipt in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER) || in.revision() < 1
            || in.tradeOrderId() == null || !in.tradeOrderId().matches("[1-9][0-9]{0,19}")
            || in.proof() == null || !in.proof().matches("[0-9a-f]{64}")
            || !Set.of("REVERSED", "DELIVERED", "MANUAL").contains(in.state()))
            throw new CommerceException(400, "扣回回执非法");
        boolean entitled = in.revision() % 2 == 0;
        if (in.state().equals(entitled ? "REVERSED" : "DELIVERED"))
            throw new CommerceException(400, "扣回回执的结论与修订号不符");
        tx.executeWithoutResult(status -> {
            var orders = jdbc.queryForList("SELECT status FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (orders.size() != 1)
                throw new CommerceException(404, "扣回订单不存在");
            var intents = jdbc.queryForList("SELECT revision,status FROM commerce_reversal_intent WHERE order_sn=? FOR UPDATE", in.orderId());
            if (intents.size() != 1)
                throw new CommerceException(404, "扣回待办不存在");
            int latest = ((Number) intents.getFirst().get("revision")).intValue();
            if (in.revision() > latest)
                throw new CommerceException(409, "扣回回执的修订号超过商城记下的最新修订号");
            var existing = jdbc.queryForList("SELECT trade_order_id,state,proof FROM commerce_reversal_receipt WHERE order_sn=? AND revision=?",
                in.orderId(), in.revision());
            if (!existing.isEmpty()) {
                var old = existing.getFirst();
                if (!in.tradeOrderId().equals(old.get("trade_order_id")) || !in.state().equals(old.get("state"))
                    || !in.proof().equals(old.get("proof")))
                    throw new CommerceException(409, "同一修订号的扣回结论发生冲突");
                return;
            }
            jdbc.update("INSERT INTO commerce_reversal_receipt(order_sn,revision,trade_order_id,state,proof) VALUES(?,?,?,?,?)",
                in.orderId(), in.revision(), in.tradeOrderId(), in.state(), in.proof());
            if (in.revision() < latest)
                return; // 之后又来了新修订号,这张回执只留档
            if (!"PENDING".equals(intents.getFirst().get("status")))
                throw new CommerceException(503, "扣回待办已终结但缺这个修订号的回执");
            jdbc.update("UPDATE commerce_reversal_intent SET status=?,updated_ms=? WHERE order_sn=? AND revision=? AND status='PENDING'",
                in.state(), clock.millis(), in.orderId(), in.revision());
            int next = switch (in.state()) { case "REVERSED" -> 9; case "DELIVERED" -> 2; default -> 7; };
            if (jdbc.update("UPDATE oms_order SET status=? WHERE order_sn=? AND status=8", next, in.orderId()) != 1)
                throw new CommerceException(503, "扣回中的订单状态不一致");
        });
    }

    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
