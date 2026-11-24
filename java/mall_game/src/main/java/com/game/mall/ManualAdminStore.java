package com.game.mall;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Clock;
import java.util.HexFormat;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import java.util.function.Supplier;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.annotation.DependsOn;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;

/**
 * 官方商城与人民币寄售的人工单(MANUAL)查询与受控处置。
 * <p>锁序与交易、付款、交付、扣回路径保持完全一致，防止死锁成环：
 * commerce_admin_command(actor, request_id) → oms_order(order_sn) → 付款回执 / 交付待办 / 扣回待办 / 退款待办 / 寄售单 → commerce_seller_guard → commerce_seller_income。
 * 每次处置都在同一事务内写入 commerce_admin_command 幂等日志，失败自动回滚，同 requestId 重放返回原提交快照。
 */
@Repository
@DependsOn("commerceSchemaReady")
public class ManualAdminStore {
    private static final Logger LOG = LoggerFactory.getLogger(ManualAdminStore.class);
    private static final int DEFAULT_LIMIT = 50;
    private static final int MAX_LIMIT = 100;
    private static final String CHANNEL_PATTERN = "[a-z][a-z0-9_-]{1,31}";
    private static final String TXN_PATTERN = "[A-Za-z0-9_-]{1,128}";
    private static final Set<String> ORDER_ACTIONS = Set.of("RETRY_REVERSAL", "MARK_REVERSED", "MARK_DELIVERED", "CREATE_REFUND");
    private static final Set<String> REFUND_ACTIONS = Set.of("RETRY_REFUND", "MARK_REFUNDED", "MARK_REVOKED");
    private static final Set<String> CONSIGNMENT_ACTIONS = Set.of("RETURN_TO_SELLER", "UNFREEZE_INCOME", "FORFEIT_INCOME");
    private static final Set<String> DISPUTE_ACTIONS = Set.of("FREEZE_DISPUTE", "RELEASE_DISPUTE");
    private static final String CASE_ID_PATTERN = "[A-Za-z0-9_-]{3,64}";

    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final ObjectMapper json;
    private final Clock clock;

    public ManualAdminStore(JdbcTemplate jdbc, PlatformTransactionManager manager, ObjectMapper json, Clock clock) {
        this.jdbc = jdbc;
        this.json = json;
        this.clock = clock;
        jdbc.setQueryTimeout(5);
        tx = new TransactionTemplate(manager);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setTimeout(5);
    }

    public ManualAdminApi.Summary list(ManualAdminApi.ListQuery input) {
        int limit = input == null || input.limit() == 0 ? DEFAULT_LIMIT : input.limit();
        if (limit < 1 || limit > MAX_LIMIT) {
            throw new CommerceException(400, "manual list limit must be between 1 and 100");
        }
        String afterOrder = input == null || input.afterOrderId() == null ? "" : input.afterOrderId();
        if (!afterOrder.isEmpty() && !afterOrder.matches(PaymentPolicy.ORDER)) {
            throw new CommerceException(400, "invalid afterOrderId cursor");
        }
        String afterListing = input == null || input.afterListingId() == null ? "" : input.afterListingId();
        if (!afterListing.isEmpty()) {
            RmbConsignmentStore.player(afterListing);
        }
        String afterRefund = input == null || input.afterRefundCursor() == null ? "" : input.afterRefundCursor();

        var orders = jdbc.query(
            "SELECT o.order_sn,o.buyer_id,o.sku_id,o.quantity,o.pay_amount,o.currency,"
                + "COALESCE(pr.revision,0) AS payment_revision,d.status AS delivery_status,"
                + "v.revision AS reversal_revision,v.status AS reversal_status,"
                + "co.listing_id,c.state AS consignment_state,"
                + "i.net_fen,i.reserved_fen,i.paid_fen,i.forfeited_fen,i.available_ms "
                + "FROM oms_order o "
                + "LEFT JOIN commerce_payment_receipt pr ON pr.order_sn=o.order_sn "
                + "LEFT JOIN commerce_delivery_intent d ON d.order_sn=o.order_sn "
                + "LEFT JOIN commerce_reversal_intent v ON v.order_sn=o.order_sn "
                + "LEFT JOIN commerce_consignment_order co ON co.order_sn=o.order_sn "
                + "LEFT JOIN commerce_consignment c ON c.listing_id=co.listing_id "
                + "LEFT JOIN commerce_seller_income i ON i.order_sn=o.order_sn "
                + "WHERE o.status=7 AND o.order_sn>? ORDER BY o.order_sn LIMIT ?",
            (r, n) -> new ManualAdminApi.ManualOrder(
                r.getString("order_sn"),
                r.getString("buyer_id"),
                r.getLong("sku_id"),
                r.getInt("quantity"),
                r.getLong("pay_amount"),
                r.getString("currency"),
                r.getInt("payment_revision"),
                r.getString("delivery_status"),
                nullableInt(r, "reversal_revision"),
                r.getString("reversal_status"),
                r.getString("listing_id"),
                r.getString("consignment_state"),
                nullableLong(r, "net_fen"),
                nullableLong(r, "reserved_fen"),
                nullableLong(r, "paid_fen"),
                nullableLong(r, "forfeited_fen"),
                nullableLong(r, "available_ms")),
            afterOrder, limit);

        var refundPage = listManualRefunds(afterRefund, limit);

        var consignments = jdbc.query(
            "SELECT c.listing_id,c.seller_player_id,c.market_zone,c.asset_kind,c.title,c.price_fen,c.current_order_sn,"
                + "o.status AS order_status,i.net_fen,i.reserved_fen,i.paid_fen,i.forfeited_fen,i.available_ms "
                + "FROM commerce_consignment c "
                + "LEFT JOIN oms_order o ON o.order_sn=c.current_order_sn "
                + "LEFT JOIN commerce_seller_income i ON i.order_sn=c.current_order_sn "
                + "WHERE c.state='MANUAL' AND c.listing_id>? ORDER BY c.listing_id LIMIT ?",
            (r, n) -> {
                Integer status = nullableInt(r, "order_status");
                return new ManualAdminApi.ManualConsignment(
                    r.getString("listing_id"),
                    r.getString("seller_player_id"),
                    r.getLong("market_zone"),
                    r.getString("asset_kind"),
                    r.getString("title"),
                    r.getLong("price_fen"),
                    r.getString("current_order_sn"),
                    status == null ? null : CommerceStore.stateName(status),
                    nullableLong(r, "net_fen"),
                    nullableLong(r, "reserved_fen"),
                    nullableLong(r, "paid_fen"),
                    nullableLong(r, "forfeited_fen"),
                    nullableLong(r, "available_ms"));
            },
            afterListing, limit);

        return new ManualAdminApi.Summary(List.copyOf(orders), refundPage.entries(), refundPage.nextCursor(), List.copyOf(consignments));
    }

    public ManualAdminApi.Resolution resolveOrder(String actor, ManualAdminApi.ResolveOrder in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.action() == null || !ORDER_ACTIONS.contains(in.action())) {
            throw new CommerceException(400, "invalid manual order resolution request");
        }
        String reason = requireReason(in.reason());
        return command(actor, in.requestId(), "manual_order", in, "ORDER", in.orderId(), () -> {
            var orders = jdbc.queryForList("SELECT status,sku_id FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (orders.size() != 1) {
                throw new CommerceException(404, "order not found");
            }
            int status = ((Number) orders.getFirst().get("status")).intValue();
            long skuId = ((Number) orders.getFirst().get("sku_id")).longValue();
            if (skuId == 0) {
                throw new CommerceException(409, "consignment orders must be resolved through manual/consignments/resolve");
            }
            if (status != 7) {
                throw new CommerceException(409, "order is not in MANUAL state");
            }
            long now = clock.millis();
            var reversals = jdbc.queryForList("SELECT revision,status FROM commerce_reversal_intent WHERE order_sn=? FOR UPDATE", in.orderId());
            String resultingState;
            switch (in.action()) {
                case "RETRY_REVERSAL" -> {
                    if (reversals.size() != 1 || !"MANUAL".equals(reversals.getFirst().get("status"))) {
                        throw new CommerceException(409, "order has no MANUAL reversal intent to retry");
                    }
                    int rev = ((Number) reversals.getFirst().get("revision")).intValue();
                    // 清除本修订号的非终局 MANUAL 回执，避免 trade 再次收敛成功回执同修订号时触发 409 冲突。
                    jdbc.update("DELETE FROM commerce_reversal_receipt WHERE order_sn=? AND revision=? AND state='MANUAL'", in.orderId(), rev);
                    if (jdbc.update("UPDATE commerce_reversal_intent SET status='PENDING',updated_ms=? WHERE order_sn=? AND revision=? AND status='MANUAL'",
                            now, in.orderId(), rev) != 1) {
                        throw new CommerceException(409, "reversal intent state changed concurrently");
                    }
                    if (jdbc.update("UPDATE oms_order SET status=8 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = CommerceStore.stateName(8);
                }
                case "MARK_REVERSED" -> {
                    if (reversals.size() != 1 || !"MANUAL".equals(reversals.getFirst().get("status"))) {
                        throw new CommerceException(409, "order has no MANUAL reversal intent");
                    }
                    int rev = ((Number) reversals.getFirst().get("revision")).intValue();
                    if (rev % 2 == 0) {
                        throw new CommerceException(409, "even payment revision is entitled and cannot be marked REVERSED");
                    }
                    if (jdbc.update("UPDATE commerce_reversal_intent SET status='REVERSED',updated_ms=? WHERE order_sn=? AND revision=? AND status='MANUAL'",
                            now, in.orderId(), rev) != 1) {
                        throw new CommerceException(409, "reversal intent state changed concurrently");
                    }
                    if (jdbc.update("UPDATE oms_order SET status=9 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = CommerceStore.stateName(9);
                }
                case "MARK_DELIVERED" -> {
                    if (!reversals.isEmpty()) {
                        if (!"MANUAL".equals(reversals.getFirst().get("status"))) {
                            throw new CommerceException(409, "order reversal intent is not in MANUAL state");
                        }
                        int rev = ((Number) reversals.getFirst().get("revision")).intValue();
                        if (rev % 2 != 0) {
                            throw new CommerceException(409, "odd payment revision is revoked and cannot be marked DELIVERED");
                        }
                        if (jdbc.update("UPDATE commerce_reversal_intent SET status='DELIVERED',updated_ms=? WHERE order_sn=? AND revision=? AND status='MANUAL'",
                                now, in.orderId(), rev) != 1) {
                            throw new CommerceException(409, "reversal intent state changed concurrently");
                        }
                    } else {
                        var deliveries = jdbc.queryForList("SELECT status FROM commerce_delivery_intent WHERE order_sn=? FOR UPDATE", in.orderId());
                        if (deliveries.size() != 1 || !"MANUAL".equals(deliveries.getFirst().get("status"))) {
                            throw new CommerceException(409, "order has no MANUAL delivery intent");
                        }
                        if (jdbc.update("UPDATE commerce_delivery_intent SET status='DELIVERED' WHERE order_sn=? AND status='MANUAL'", in.orderId()) != 1) {
                            throw new CommerceException(409, "delivery intent state changed concurrently");
                        }
                    }
                    if (jdbc.update("UPDATE oms_order SET status=2 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = CommerceStore.stateName(2);
                }
                case "CREATE_REFUND" -> {
                    if (!reversals.isEmpty()) {
                        throw new CommerceException(409, "orders governed by payment reversal cannot create a new refund intent");
                    }
                    var deliveries = jdbc.queryForList("SELECT status FROM commerce_delivery_intent WHERE order_sn=? FOR UPDATE", in.orderId());
                    if (deliveries.size() != 1 || !"MANUAL".equals(deliveries.getFirst().get("status"))) {
                        throw new CommerceException(409, "CREATE_REFUND requires a MANUAL delivery intent");
                    }
                    var receipts = jdbc.queryForList("SELECT channel,transaction_id,fact FROM commerce_payment_receipt WHERE order_sn=? FOR UPDATE", in.orderId());
                    if (receipts.size() != 1) {
                        throw new CommerceException(503, "bound payment receipt missing for official order");
                    }
                    if (!PaymentRevisionStore.PAID.equals(receipts.getFirst().get("fact"))) {
                        throw new CommerceException(409, "bound payment is not PAID");
                    }
                    byte[] channelBytes = (byte[]) receipts.getFirst().get("channel");
                    byte[] txnBytes = (byte[]) receipts.getFirst().get("transaction_id");
                    var existingRefund = jdbc.queryForList(
                        "SELECT status FROM commerce_refund_intent WHERE channel=? AND transaction_id=? FOR UPDATE",
                        channelBytes, txnBytes);
                    if (!existingRefund.isEmpty()) {
                        throw new CommerceException(409, "refund intent already exists; resolve it through manual/refunds/resolve");
                    }
                    jdbc.update("INSERT INTO commerce_refund_intent(channel,transaction_id,order_sn,source,status) VALUES(?,?,?,?,?)",
                        channelBytes, txnBytes, in.orderId(), CommerceApi.RefundReason.DELIVERY_REJECTED.name(), "PENDING");
                    if (jdbc.update("UPDATE commerce_delivery_intent SET status='REFUND_PENDING' WHERE order_sn=? AND status='MANUAL'", in.orderId()) != 1) {
                        throw new CommerceException(409, "delivery intent state changed concurrently");
                    }
                    if (jdbc.update("UPDATE oms_order SET status=5 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = CommerceStore.stateName(5);
                }
                default -> throw new CommerceException(400, "unsupported manual order action");
            }
            afterCommit(() -> LOG.warn("manual official order resolved: actor={} order={} action={} resultingState={} reason={}",
                actor, in.orderId(), in.action(), resultingState, reason));
            return new ManualAdminApi.Resolution("ORDER", in.orderId(), in.action(), resultingState, reason, now);
        });
    }

    public ManualAdminApi.Resolution resolveRefund(String actor, ManualAdminApi.ResolveRefund in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.channel() == null || !in.channel().matches(CHANNEL_PATTERN)
            || in.paymentTransactionId() == null || !in.paymentTransactionId().matches(TXN_PATTERN)
            || in.action() == null || !REFUND_ACTIONS.contains(in.action())) {
            throw new CommerceException(400, "invalid manual refund resolution request");
        }
        String reason = requireReason(in.reason());
        if ("MARK_REFUNDED".equals(in.action())) {
            if (in.externalRefundId() == null || !in.externalRefundId().matches(TXN_PATTERN)) {
                throw new CommerceException(400, "MARK_REFUNDED requires a valid externalRefundId");
            }
        } else if (in.externalRefundId() != null && !in.externalRefundId().isEmpty()) {
            throw new CommerceException(400, "externalRefundId is only accepted for MARK_REFUNDED");
        }
        String targetId = in.orderId() + ":" + in.channel() + ":" + in.paymentTransactionId();
        return command(actor, in.requestId(), "manual_refund", in, "REFUND", targetId, () -> {
            var orders = jdbc.queryForList("SELECT status FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (orders.size() != 1) {
                throw new CommerceException(404, "order not found");
            }
            int orderStatus = ((Number) orders.getFirst().get("status")).intValue();
            byte[] channelBytes = ascii(in.channel());
            byte[] paymentTxnBytes = ascii(in.paymentTransactionId());
            var intents = jdbc.queryForList(
                "SELECT order_sn,source,status FROM commerce_refund_intent WHERE channel=? AND transaction_id=? FOR UPDATE",
                channelBytes, paymentTxnBytes);
            if (intents.size() != 1 || !in.orderId().equals(intents.getFirst().get("order_sn"))) {
                throw new CommerceException(404, "refund intent not found");
            }
            if (!"MANUAL".equals(intents.getFirst().get("status"))) {
                throw new CommerceException(409, "refund intent is not in MANUAL state");
            }
            var receipts = jdbc.queryForList(
                "SELECT order_sn,amount_minor,currency,fact FROM commerce_payment_receipt WHERE channel=? AND transaction_id=? FOR UPDATE",
                channelBytes, paymentTxnBytes);
            if (receipts.size() != 1) {
                throw new CommerceException(503, "payment receipt missing for refund intent");
            }
            var receipt = receipts.getFirst();
            boolean ordersOwnPayment = CommerceApi.RefundReason.stored((String) intents.getFirst().get("source")).refundsOrdersOwnPayment();
            if (ordersOwnPayment != in.orderId().equals(receipt.get("order_sn"))) {
                throw new CommerceException(503, "refund intent source does not match payment binding");
            }
            if (ordersOwnPayment && orderStatus != 7) {
                throw new CommerceException(409, "linked order is not in MANUAL state");
            }
            long now = clock.millis();
            String resultingState;
            switch (in.action()) {
                case "RETRY_REFUND" -> {
                    if (!PaymentRevisionStore.PAID.equals(receipt.get("fact"))) {
                        throw new CommerceException(409, "payment fact is not PAID and cannot be retried for refund");
                    }
                    if (jdbc.update("UPDATE commerce_refund_intent SET status='PENDING',close_reason=NULL WHERE channel=? AND transaction_id=? AND status='MANUAL'",
                            channelBytes, paymentTxnBytes) != 1) {
                        throw new CommerceException(409, "refund intent state changed concurrently");
                    }
                    if (ordersOwnPayment && jdbc.update("UPDATE oms_order SET status=5 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = "PENDING";
                }
                case "MARK_REFUNDED" -> {
                    var prior = jdbc.queryForList(
                        "SELECT transaction_id FROM commerce_refund_receipt WHERE channel=? AND payment_transaction_id=?",
                        channelBytes, paymentTxnBytes);
                    if (!prior.isEmpty()) {
                        throw new CommerceException(409, "refund receipt already recorded for this payment");
                    }
                    long amountMinor = ((Number) receipt.get("amount_minor")).longValue();
                    String currency = (String) receipt.get("currency");
                    jdbc.update(
                        "INSERT INTO commerce_refund_receipt(channel,payment_transaction_id,transaction_id,order_sn,amount_minor,currency) VALUES(?,?,?,?,?,?)",
                        channelBytes, paymentTxnBytes, ascii(in.externalRefundId()), in.orderId(), amountMinor, currency);
                    if (jdbc.update("UPDATE commerce_refund_intent SET status='REFUNDED',close_reason='MANUAL_REFUNDED' WHERE channel=? AND transaction_id=? AND status='MANUAL'",
                            channelBytes, paymentTxnBytes) != 1) {
                        throw new CommerceException(409, "refund intent state changed concurrently");
                    }
                    if (ordersOwnPayment && jdbc.update("UPDATE oms_order SET status=6 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = "REFUNDED";
                }
                case "MARK_REVOKED" -> {
                    if (jdbc.update("UPDATE commerce_refund_intent SET status='REVOKED',close_reason='MANUAL_REVOKED' WHERE channel=? AND transaction_id=? AND status='MANUAL'",
                            channelBytes, paymentTxnBytes) != 1) {
                        throw new CommerceException(409, "refund intent state changed concurrently");
                    }
                    if (ordersOwnPayment && jdbc.update("UPDATE oms_order SET status=6 WHERE order_sn=? AND status=7", in.orderId()) != 1) {
                        throw new CommerceException(409, "order state changed concurrently");
                    }
                    resultingState = "REVOKED";
                }
                default -> throw new CommerceException(400, "unsupported manual refund action");
            }
            afterCommit(() -> LOG.warn("manual refund intent resolved: actor={} order={} channel={} paymentTxn={} action={} resultingState={} reason={}",
                actor, in.orderId(), in.channel(), in.paymentTransactionId(), in.action(), resultingState, reason));
            return new ManualAdminApi.Resolution("REFUND", targetId, in.action(), resultingState, reason, now);
        });
    }

    public ManualAdminApi.Resolution resolveConsignment(String actor, ManualAdminApi.ResolveConsignment in) {
        if (in == null || in.listingId() == null || in.action() == null || !CONSIGNMENT_ACTIONS.contains(in.action())) {
            throw new CommerceException(400, "invalid manual consignment resolution request");
        }
        RmbConsignmentStore.player(in.listingId());
        String reason = requireReason(in.reason());
        return command(actor, in.requestId(), "manual_consign", in, "CONSIGNMENT", in.listingId(), () -> {
            // 先非加锁探查 current_order_sn，以便按「oms_order → commerce_consignment → commerce_seller_guard → commerce_seller_income」
            // 全局一致锁序获取行锁，锁定后再复核 current_order_sn 未漂移。
            var peek = jdbc.queryForList("SELECT current_order_sn FROM commerce_consignment WHERE listing_id=?", in.listingId());
            if (peek.size() != 1) {
                throw new CommerceException(404, "consignment listing not found");
            }
            String observedOrderId = (String) peek.getFirst().get("current_order_sn");
            Integer lockedOrderStatus = null;
            if (observedOrderId != null) {
                var orderRows = jdbc.queryForList("SELECT status,sku_id FROM oms_order WHERE order_sn=? FOR UPDATE", observedOrderId);
                if (orderRows.size() != 1 || ((Number) orderRows.getFirst().get("sku_id")).longValue() != 0) {
                    throw new CommerceException(503, "consignment current order record inconsistent");
                }
                lockedOrderStatus = ((Number) orderRows.getFirst().get("status")).intValue();
            }
            var listings = jdbc.queryForList("SELECT state,current_order_sn FROM commerce_consignment WHERE listing_id=? FOR UPDATE", in.listingId());
            if (listings.size() != 1) {
                throw new CommerceException(404, "consignment listing not found");
            }
            String state = (String) listings.getFirst().get("state");
            String currentOrderId = (String) listings.getFirst().get("current_order_sn");
            if (!Objects.equals(observedOrderId, currentOrderId)) {
                throw new CommerceException(409, "consignment current order changed concurrently, please retry");
            }
            if (!"MANUAL".equals(state)) {
                throw new CommerceException(409, "consignment is not in MANUAL state");
            }
            long now = clock.millis();
            String resultingState;
            switch (in.action()) {
                case "RETURN_TO_SELLER" -> {
                    if (currentOrderId != null) {
                        var incomes = jdbc.queryForList("SELECT order_sn FROM commerce_seller_income WHERE order_sn=?", currentOrderId);
                        if (!incomes.isEmpty()) {
                            throw new CommerceException(409, "asset already delivered and seller income recorded; use UNFREEZE_INCOME or FORFEIT_INCOME");
                        }
                        var intents = jdbc.queryForList("SELECT status FROM commerce_delivery_intent WHERE order_sn=? FOR UPDATE", currentOrderId);
                        if (!intents.isEmpty()) {
                            String intentStatus = (String) intents.getFirst().get("status");
                            if ("DELIVERED".equals(intentStatus)) {
                                throw new CommerceException(409, "consignment delivery already DELIVERED; cannot return asset to seller");
                            }
                            if ("PENDING".equals(intentStatus) || "MANUAL".equals(intentStatus)) {
                                jdbc.update("UPDATE commerce_delivery_intent SET status='REVOKED' WHERE order_sn=? AND status=?",
                                    currentOrderId, intentStatus);
                            }
                        }
                        if (lockedOrderStatus != null && lockedOrderStatus == 7) {
                            jdbc.update("UPDATE oms_order SET status=9 WHERE order_sn=? AND status=7", currentOrderId);
                        }
                    }
                    if (jdbc.update("UPDATE commerce_consignment SET state='RETURNING' WHERE listing_id=? AND state='MANUAL'", in.listingId()) != 1) {
                        throw new CommerceException(409, "consignment state changed concurrently");
                    }
                    resultingState = "RETURNING";
                }
                case "UNFREEZE_INCOME" -> {
                    if (currentOrderId == null) {
                        throw new CommerceException(409, "consignment has no bound order for income unfreeze");
                    }
                    var sellerAccounts = jdbc.query("SELECT account FROM commerce_seller_income WHERE order_sn=?",
                        (row, n) -> row.getBytes(1), currentOrderId);
                    if (sellerAccounts.size() != 1) {
                        throw new CommerceException(409, "consignment has no seller income record to unfreeze");
                    }
                    lockSeller(sellerAccounts.getFirst());
                    var incomeRows = jdbc.queryForList(
                        "SELECT net_fen,reserved_fen,paid_fen,forfeited_fen,available_ms FROM commerce_seller_income WHERE order_sn=? FOR UPDATE",
                        currentOrderId);
                    if (incomeRows.size() != 1) {
                        throw new CommerceException(503, "seller income record missing under seller lock");
                    }
                    var income = incomeRows.getFirst();
                    long net = ((Number) income.get("net_fen")).longValue();
                    long reserved = ((Number) income.get("reserved_fen")).longValue();
                    long paid = ((Number) income.get("paid_fen")).longValue();
                    long forfeited = ((Number) income.get("forfeited_fen")).longValue();
                    long availableMs = ((Number) income.get("available_ms")).longValue();
                    if (availableMs != Long.MAX_VALUE) {
                        throw new CommerceException(409, "seller income is not frozen");
                    }
                    if (net - reserved - paid - forfeited <= 0) {
                        throw new CommerceException(409, "seller income has no remaining balance to unfreeze");
                    }
                    if (jdbc.update("UPDATE commerce_seller_income SET available_ms=? WHERE order_sn=? AND available_ms=?",
                            now, currentOrderId, Long.MAX_VALUE) != 1) {
                        throw new CommerceException(409, "seller income changed concurrently");
                    }
                    if (jdbc.update("UPDATE commerce_consignment SET state='SOLD' WHERE listing_id=? AND state='MANUAL'", in.listingId()) != 1) {
                        throw new CommerceException(409, "consignment state changed concurrently");
                    }
                    if (lockedOrderStatus != null && lockedOrderStatus == 7) {
                        jdbc.update("UPDATE oms_order SET status=2 WHERE order_sn=? AND status=7", currentOrderId);
                    }
                    resultingState = "SOLD";
                }
                case "FORFEIT_INCOME" -> {
                    if (currentOrderId == null) {
                        throw new CommerceException(409, "consignment has no bound order for income forfeiture");
                    }
                    var sellerAccounts = jdbc.query("SELECT account FROM commerce_seller_income WHERE order_sn=?",
                        (row, n) -> row.getBytes(1), currentOrderId);
                    if (sellerAccounts.size() != 1) {
                        throw new CommerceException(409, "consignment has no seller income record to forfeit");
                    }
                    lockSeller(sellerAccounts.getFirst());
                    var incomeRows = jdbc.queryForList(
                        "SELECT net_fen,reserved_fen,paid_fen,forfeited_fen FROM commerce_seller_income WHERE order_sn=? FOR UPDATE",
                        currentOrderId);
                    if (incomeRows.size() != 1) {
                        throw new CommerceException(503, "seller income record missing under seller lock");
                    }
                    var income = incomeRows.getFirst();
                    long net = ((Number) income.get("net_fen")).longValue();
                    long reserved = ((Number) income.get("reserved_fen")).longValue();
                    long paid = ((Number) income.get("paid_fen")).longValue();
                    long forfeited = ((Number) income.get("forfeited_fen")).longValue();
                    if (reserved > 0) {
                        throw new CommerceException(409, "in-flight withdrawal reserves this income; settle withdrawal before forfeiting");
                    }
                    long remaining = net - paid - forfeited;
                    if (remaining < 0) {
                        throw new CommerceException(503, "seller income accounting invariant broken");
                    }
                    if (jdbc.update(
                            "UPDATE commerce_seller_income SET forfeited_fen=forfeited_fen+?,dispute_frozen_fen=0,dispute_case_id=NULL,available_ms=? "
                                + "WHERE order_sn=? AND reserved_fen=0 AND paid_fen+forfeited_fen+?=net_fen",
                            remaining, Long.MAX_VALUE, currentOrderId, remaining) != 1) {
                        throw new CommerceException(409, "seller income changed concurrently");
                    }
                    if (jdbc.update("UPDATE commerce_consignment SET state='FORFEITED' WHERE listing_id=? AND state='MANUAL'", in.listingId()) != 1) {
                        throw new CommerceException(409, "consignment state changed concurrently");
                    }
                    if (lockedOrderStatus != null && lockedOrderStatus == 7) {
                        jdbc.update("UPDATE oms_order SET status=9 WHERE order_sn=? AND status=7", currentOrderId);
                    }
                    resultingState = "FORFEITED";
                }
                default -> throw new CommerceException(400, "unsupported manual consignment action");
            }
            afterCommit(() -> LOG.warn("manual consignment resolved: actor={} listing={} order={} action={} resultingState={} reason={}",
                actor, in.listingId(), currentOrderId, in.action(), resultingState, reason));
            return new ManualAdminApi.Resolution("CONSIGNMENT", in.listingId(), in.action(), resultingState, reason, now);
        });
    }

    /**
     * 逐单争议冻结 / 释放卖家收入。
     * 锁序严格沿用 commerce_admin_command → oms_order → commerce_seller_guard → commerce_seller_income，
     * 与 RmbConsignmentStore.withdraw 共用同一账号锁，彻底防止争议与并发提现竞态双花。
     */
    public ManualAdminApi.Resolution resolveIncomeDispute(String actor, ManualAdminApi.ResolveIncomeDispute in) {
        if (in == null || in.orderId() == null || !in.orderId().matches(PaymentPolicy.ORDER)
            || in.action() == null || !DISPUTE_ACTIONS.contains(in.action())
            || in.amountFen() < 1 || in.amountFen() > CommerceStore.MAX_AMOUNT_MINOR
            || in.caseId() == null || !in.caseId().matches(CASE_ID_PATTERN)) {
            throw new CommerceException(400, "invalid income dispute request");
        }
        String reason = requireReason(in.reason());
        return command(actor, in.requestId(), "income_dispute", in, "INCOME_DISPUTE", in.orderId(), () -> {
            var orderRows = jdbc.queryForList("SELECT status,sku_id FROM oms_order WHERE order_sn=? FOR UPDATE", in.orderId());
            if (orderRows.size() != 1) {
                throw new CommerceException(404, "order not found");
            }
            if (((Number) orderRows.getFirst().get("sku_id")).longValue() != 0) {
                throw new CommerceException(409, "dispute freeze only applies to consignment orders");
            }
            var sellerAccounts = jdbc.query("SELECT account FROM commerce_seller_income WHERE order_sn=?",
                (row, n) -> row.getBytes(1), in.orderId());
            if (sellerAccounts.size() != 1) {
                throw new CommerceException(404, "seller income record not found");
            }
            lockSeller(sellerAccounts.getFirst());
            var incomeRows = jdbc.queryForList(
                "SELECT net_fen,reserved_fen,paid_fen,forfeited_fen,dispute_frozen_fen,dispute_case_id "
                    + "FROM commerce_seller_income WHERE order_sn=? FOR UPDATE",
                in.orderId());
            if (incomeRows.size() != 1) {
                throw new CommerceException(503, "seller income record missing under seller lock");
            }
            var income = incomeRows.getFirst();
            long net = ((Number) income.get("net_fen")).longValue();
            long reserved = ((Number) income.get("reserved_fen")).longValue();
            long paid = ((Number) income.get("paid_fen")).longValue();
            long forfeited = ((Number) income.get("forfeited_fen")).longValue();
            long disputeFrozen = ((Number) income.get("dispute_frozen_fen")).longValue();
            long now = clock.millis();
            String resultingState;
            switch (in.action()) {
                case "FREEZE_DISPUTE" -> {
                    long availableToFreeze = net - reserved - paid - forfeited - disputeFrozen;
                    if (availableToFreeze < in.amountFen()) {
                        throw new CommerceException(409, "free seller income insufficient to freeze; funds may already be reserved or paid out");
                    }
                    if (jdbc.update(
                            "UPDATE commerce_seller_income SET dispute_frozen_fen=dispute_frozen_fen+?,dispute_case_id=? "
                                + "WHERE order_sn=? AND net_fen-reserved_fen-paid_fen-forfeited_fen-dispute_frozen_fen>=?",
                            in.amountFen(), in.caseId(), in.orderId(), in.amountFen()) != 1) {
                        throw new CommerceException(409, "seller income changed concurrently");
                    }
                    resultingState = "DISPUTE_FROZEN:" + (disputeFrozen + in.amountFen());
                }
                case "RELEASE_DISPUTE" -> {
                    if (disputeFrozen < in.amountFen()) {
                        throw new CommerceException(409, "dispute release exceeds currently frozen amount");
                    }
                    long remainingFrozen = disputeFrozen - in.amountFen();
                    String nextCase = remainingFrozen == 0 ? null : in.caseId();
                    if (jdbc.update(
                            "UPDATE commerce_seller_income SET dispute_frozen_fen=dispute_frozen_fen-?,dispute_case_id=? "
                                + "WHERE order_sn=? AND dispute_frozen_fen>=?",
                            in.amountFen(), nextCase, in.orderId(), in.amountFen()) != 1) {
                        throw new CommerceException(409, "seller income changed concurrently");
                    }
                    resultingState = remainingFrozen == 0 ? "DISPUTE_RELEASED" : "DISPUTE_FROZEN:" + remainingFrozen;
                }
                default -> throw new CommerceException(400, "unsupported income dispute action");
            }
            afterCommit(() -> LOG.warn("seller income dispute updated: actor={} order={} action={} amountFen={} caseId={} resultingState={} reason={}",
                actor, in.orderId(), in.action(), in.amountFen(), in.caseId(), resultingState, reason));
            return new ManualAdminApi.Resolution("INCOME_DISPUTE", in.orderId(), in.action(), resultingState, reason, now);
        });
    }

    private record RefundPage(List<ManualAdminApi.ManualRefund> entries, String nextCursor) {}

    private RefundPage listManualRefunds(String rawCursor, int limit) {
        List<ManualAdminApi.ManualRefund> rows;
        if (rawCursor.isEmpty()) {
            rows = jdbc.query(
                "SELECT r.order_sn,p.channel,p.transaction_id,p.amount_minor,p.currency,r.source,r.close_reason,p.revision,p.fact "
                    + "FROM commerce_refund_intent r "
                    + "JOIN commerce_payment_receipt p ON p.channel=r.channel AND p.transaction_id=r.transaction_id "
                    + "WHERE r.status='MANUAL' ORDER BY r.order_sn,r.channel,r.transaction_id LIMIT ?",
                (r, n) -> manualRefund(r), limit + 1);
        } else {
            String[] parts = rawCursor.split(":", -1);
            if (parts.length != 4 || !"r1".equals(parts[0])
                || !parts[1].matches(PaymentPolicy.ORDER)
                || !parts[2].matches(CHANNEL_PATTERN)
                || !parts[3].matches(TXN_PATTERN)) {
                throw new CommerceException(400, "invalid afterRefundCursor");
            }
            byte[] channelBytes = ascii(parts[2]);
            byte[] txnBytes = ascii(parts[3]);
            rows = jdbc.query(
                "SELECT r.order_sn,p.channel,p.transaction_id,p.amount_minor,p.currency,r.source,r.close_reason,p.revision,p.fact "
                    + "FROM commerce_refund_intent r "
                    + "JOIN commerce_payment_receipt p ON p.channel=r.channel AND p.transaction_id=r.transaction_id "
                    + "WHERE r.status='MANUAL' AND (r.order_sn>? OR (r.order_sn=? AND (r.channel>? OR (r.channel=? AND r.transaction_id>?)))) "
                    + "ORDER BY r.order_sn,r.channel,r.transaction_id LIMIT ?",
                (r, n) -> manualRefund(r), parts[1], parts[1], channelBytes, channelBytes, txnBytes, limit + 1);
        }
        boolean hasMore = rows.size() > limit;
        var entries = hasMore ? List.copyOf(rows.subList(0, limit)) : List.copyOf(rows);
        String nextCursor = "";
        if (hasMore && !entries.isEmpty()) {
            var last = entries.getLast();
            nextCursor = "r1:" + last.orderId() + ":" + last.channel() + ":" + last.transactionId();
        }
        return new RefundPage(entries, nextCursor);
    }

    private static ManualAdminApi.ManualRefund manualRefund(ResultSet r) throws SQLException {
        return new ManualAdminApi.ManualRefund(
            r.getString("order_sn"),
            new String(r.getBytes("channel"), StandardCharsets.US_ASCII),
            new String(r.getBytes("transaction_id"), StandardCharsets.US_ASCII),
            r.getLong("amount_minor"),
            r.getString("currency"),
            CommerceApi.RefundReason.stored(r.getString("source")),
            r.getString("close_reason"),
            r.getInt("revision"),
            r.getString("fact"));
    }

    private ManualAdminApi.Resolution command(
        String actor,
        String requestId,
        String kind,
        Object input,
        String targetType,
        String targetId,
        Supplier<ManualAdminApi.Resolution> mutation) {
        if (actor == null || !actor.matches("[a-z][a-z0-9-]{2,63}")
            || requestId == null || !requestId.matches("[A-Za-z0-9_-]{8,128}")) {
            throw new CommerceException(400, "invalid admin actor or requestId");
        }
        String digest = digest(kind, input);
        byte[] actorKey = ascii(actor);
        byte[] requestKey = ascii(requestId);
        String pending = "pending:" + UUID.randomUUID();
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                jdbc.update(
                    "INSERT INTO commerce_admin_command(actor,request_id,command_type,request_digest,result_json,created_ms) "
                        + "VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE request_id=request_id",
                    actorKey, requestKey, kind, digest, pending, clock.millis());
                var rows = jdbc.queryForList(
                    "SELECT command_type,request_digest,result_json FROM commerce_admin_command WHERE actor=? AND request_id=? FOR UPDATE",
                    actorKey, requestKey);
                if (rows.size() != 1) {
                    throw new CommerceException(503, "admin command journal row missing");
                }
                var stored = rows.getFirst();
                if (!kind.equals(stored.get("command_type")) || !digest.equals(stored.get("request_digest"))) {
                    throw new CommerceException(409, "requestId already used for a different admin command");
                }
                if (!pending.equals(stored.get("result_json"))) {
                    if (stored.get("result_json") == null) {
                        throw new CommerceException(503, "admin command stored response missing");
                    }
                    var replay = decode(stored.get("result_json").toString());
                    if (!targetType.equals(replay.targetType()) || !targetId.equals(replay.targetId())) {
                        throw new CommerceException(503, "admin command stored response target mismatch");
                    }
                    return replay;
                }
                var result = mutation.get();
                if (jdbc.update("UPDATE commerce_admin_command SET result_json=? WHERE actor=? AND request_id=? AND result_json=?",
                        encode(result), actorKey, requestKey, pending) != 1) {
                    throw new CommerceException(503, "failed to persist admin command response");
                }
                return result;
            }));
        } catch (DuplicateKeyException conflict) {
            throw new CommerceException(409, "unique receipt or admin command conflict");
        }
    }

    private void lockSeller(byte[] sellerAccount) {
        jdbc.update("INSERT INTO commerce_seller_guard(account) VALUES(?) ON DUPLICATE KEY UPDATE account=account", sellerAccount);
        jdbc.queryForObject("SELECT account FROM commerce_seller_guard WHERE account=? FOR UPDATE", byte[].class, sellerAccount);
    }

    private static String requireReason(String reason) {
        if (reason == null || reason.isBlank() || reason.length() > 256) {
            throw new CommerceException(400, "reason must be 1..256 non-blank characters");
        }
        return reason.trim();
    }

    private String digest(String kind, Object input) {
        try {
            byte[] body = (kind + "\n" + json.writeValueAsString(input)).getBytes(StandardCharsets.UTF_8);
            return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(body));
        } catch (JsonProcessingException | NoSuchAlgorithmException error) {
            throw new IllegalStateException("failed to hash manual admin request", error);
        }
    }

    private String encode(ManualAdminApi.Resolution result) {
        try {
            return json.writeValueAsString(result);
        } catch (JsonProcessingException error) {
            throw new IllegalStateException("failed to encode manual admin response", error);
        }
    }

    private ManualAdminApi.Resolution decode(String raw) {
        try {
            return json.readerFor(ManualAdminApi.Resolution.class)
                .with(DeserializationFeature.FAIL_ON_TRAILING_TOKENS).readValue(raw);
        } catch (JsonProcessingException corrupt) {
            throw new CommerceException(503, "corrupt manual admin command response");
        }
    }

    private static void afterCommit(Runnable log) {
        TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
            @Override public void afterCommit() { log.run(); }
        });
    }

    private static Integer nullableInt(ResultSet r, String column) throws SQLException {
        Object value = r.getObject(column);
        return value == null ? null : ((Number) value).intValue();
    }

    private static Long nullableLong(ResultSet r, String column) throws SQLException {
        Object value = r.getObject(column);
        return value == null ? null : ((Number) value).longValue();
    }

    private static byte[] ascii(String value) {
        return value.getBytes(StandardCharsets.US_ASCII);
    }
}
