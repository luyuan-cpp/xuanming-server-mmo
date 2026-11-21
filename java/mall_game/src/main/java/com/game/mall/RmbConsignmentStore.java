package com.game.mall;

import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Clock;
import java.util.Arrays;
import java.util.HexFormat;
import java.util.List;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.DependsOn;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

/** 只拥有寄售关联与人民币结算；OMS订单由CommerceStore创建、支付和取消。 */
@Repository
@DependsOn("commerceSchemaReady")
// 保留Repository异常翻译的类代理，与同域CommerceStore一致。
public class RmbConsignmentStore {
    /** 验证适配器自己的真实持久出款事实；缺实现时不能把调用方声明当渠道证明。 */
    public interface PayoutEvidenceVerifier {
        void verify(JdbcTemplate transaction, RmbConsignmentApi.WithdrawalReceipt receipt);
    }
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final Clock clock;
    private final RmbConsignmentProperties rules;
    private final PayoutEvidenceVerifier payouts;
    private final ObjectMapper json = new ObjectMapper();

    @Autowired public RmbConsignmentStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock,
            RmbConsignmentProperties rules, ObjectProvider<PayoutEvidenceVerifier> payouts) {
        this(jdbc, manager, clock, rules, payouts.getIfAvailable());
    }
    RmbConsignmentStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock,
            RmbConsignmentProperties rules, PayoutEvidenceVerifier payouts) {
        this.jdbc = jdbc; this.clock = clock; this.rules = rules; this.payouts = payouts;
        tx = new TransactionTemplate(manager);
        tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED); tx.setTimeout(5);
        jdbc.setQueryTimeout(5);
    }
    record Frozen(String listingId, String sellerAccount, String buyerAccount, String buyerOwnerRevision, String escrowReference,
                  String escrowProof, long priceFen, int feeBps, long coolingMs, String title) {}
    private record Stored(RmbConsignmentApi.Listing view, String sellerAccount, String escrowReference,
                          String escrowProof, String digest) {}

    public RmbConsignmentApi.Listing register(RmbConsignmentApi.Escrow input) {
        if (input == null) { throw invalid(); }
        player(input.listingId()); player(input.sellerPlayerId()); player(input.escrowReference());
        account(input.sellerAccount()); proof(input.escrowProof());
        if (input.marketZone() < 1 || input.marketZone() > 0xffff_ffffL || input.assetKind() == null
            || !Set.of("ITEMS", "CURRENCY", "PET", "CHARACTER").contains(input.assetKind())
            || input.priceFen() < 1 || input.priceFen() > CommerceStore.MAX_AMOUNT_MINOR || input.title() == null
            || input.title().isBlank() || input.title().length() > 128) { throw invalid(); }
        if (input.assetKind().equals("CURRENCY")) {
            if (input.assetCurrencyType() == null || !rules.getCurrencies().contains(input.assetCurrencyType())) { throw invalid(); }
        } else if (input.assetCurrencyType() != null) { throw invalid(); }
        String digest = digest(input);
        long now = clock.millis();
        long notice = Math.addExact(now, rules.getNoticeMs());
        long end = Math.addExact(notice, rules.getSaleMs());
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                // listing主键和唯一托管引用共同串行化首次登记，不接管不同资产的旧listing。
                jdbc.update("INSERT INTO commerce_consignment(listing_id,seller_player_id,seller_account,market_zone,asset_kind,"
                    + "asset_currency_type,escrow_reference,escrow_proof,request_digest,title,price_fen,fee_bps,cooling_ms,"
                    + "notice_end_ms,sale_end_ms,state,created_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'LISTED',?)"
                    + " ON DUPLICATE KEY UPDATE listing_id=listing_id", input.listingId(), input.sellerPlayerId(),
                    bytes(input.sellerAccount()), input.marketZone(), input.assetKind(), input.assetCurrencyType(),
                    input.escrowReference(), input.escrowProof(), digest, input.title(), input.priceFen(), rules.getFeeBps(),
                    rules.getCoolingMs(), notice, end, now);
                var rows = listing(input.listingId(), true);
                if (rows.isEmpty() || !rows.getFirst().digest().equals(digest)) {
                    throw new CommerceException(409, "寄售编号或托管引用已用于不同条款");
                }
                return rows.getFirst().view();
            }));
        } catch (DuplicateKeyException duplicate) { throw new CommerceException(409, "托管合同已登记其它寄售"); }
    }

    public RmbConsignmentApi.Listings list(RmbConsignmentApi.Page page) {
        if (page == null || page.afterListingId() == null || page.limit() < 1 || page.limit() > 100) { throw invalid(); }
        if (!page.afterListingId().isEmpty()) { player(page.afterListingId()); }
        return new RmbConsignmentApi.Listings(jdbc.query("SELECT * FROM commerce_consignment WHERE listing_id>? "
            + "AND state IN ('LISTED','LOCKED') AND sale_end_ms>? ORDER BY listing_id LIMIT ?",
            (row, index) -> stored(row).view(), page.afterListingId(), clock.millis(), page.limit()));
    }
    public RmbConsignmentApi.Listing get(RmbConsignmentApi.Get input) {
        if (input == null) { throw invalid(); }
        player(input.listingId());
        var rows = listing(input.listingId(), false);
        if (rows.isEmpty()) { throw new CommerceException(404, "寄售不存在"); }
        return rows.getFirst().view();
    }

    public RmbConsignmentApi.Listing cancel(RmbConsignmentApi.Cancel input) {
        if (input == null) { throw invalid(); }
        player(input.listingId()); account(input.sellerAccount());
        return Objects.requireNonNull(tx.execute(status -> {
            var value = requireListing(input.listingId());
            if (!value.sellerAccount().equals(input.sellerAccount())) { throw new CommerceException(404, "寄售不存在"); }
            if (Set.of("RETURNING", "RETURNED").contains(value.view().state())) { return value.view(); }
            if (!value.view().state().equals("LISTED")) { throw new CommerceException(409, "交易中的寄售不能下架"); }
            jdbc.update("UPDATE commerce_consignment SET state='RETURNING' WHERE listing_id=? AND state='LISTED'", input.listingId());
            return requireListing(input.listingId()).view();
        }));
    }

    // 下面的订单钩子只能在CommerceStore当前OMS事务内调用，固定buyer guard→listing→新订单锁序。
    Frozen reserve(RmbConsignmentApi.Buy input, String orderId) {
        if (input == null) { throw invalid(); }
        player(input.listingId()); player(input.buyerPlayerId()); account(input.buyerAccount()); request(input.requestId());
        CommerceIdentity.revision(input.buyerOwnerRevision());
        var value = requireListing(input.listingId());
        if (value.sellerAccount().equals(input.buyerAccount())) { throw new CommerceException(409, "同账号不能购买自己的角色或资产"); }
        long now = clock.millis();
        if (!value.view().state().equals("LISTED") || now < value.view().noticeEndMs() || now >= value.view().saleEndMs()) {
            throw new CommerceException(409, "寄售仍在公示、已结束或已被占用");
        }
        if (jdbc.update("UPDATE commerce_consignment SET state='LOCKED',current_order_sn=? WHERE listing_id=? AND state='LISTED'",
                orderId, input.listingId()) != 1) { throw new CommerceException(409, "寄售状态已变化"); }
        var view = value.view();
        jdbc.update("INSERT INTO commerce_consignment_order(order_sn,listing_id,buyer_account,buyer_owner_revision,request_id,seller_account,escrow_reference,"
            + "escrow_proof,fee_bps,cooling_ms) VALUES(?,?,?,?,?,?,?,?,?,?)", orderId, input.listingId(), bytes(input.buyerAccount()), bytes(input.buyerOwnerRevision()), bytes(input.requestId()),
            bytes(value.sellerAccount()), value.escrowReference(), value.escrowProof(), view.feeBps(), view.coolingMs());
        return new Frozen(view.listingId(), value.sellerAccount(), input.buyerAccount(), input.buyerOwnerRevision(), value.escrowReference(),
            value.escrowProof(), view.priceFen(), view.feeBps(), view.coolingMs(), view.title());
    }

    Frozen mapping(String orderId) {
        var rows = jdbc.query("SELECT c.*,o.pay_amount,l.title FROM commerce_consignment_order c JOIN oms_order o ON o.order_sn=c.order_sn "
            + "JOIN commerce_consignment l ON l.listing_id=c.listing_id WHERE c.order_sn=?", (row, index) -> new Frozen(
                row.getString("listing_id"), text(row.getBytes("seller_account")), CommerceIdentity.storedAccount(row.getBytes("buyer_account")),
                CommerceIdentity.storedRevision(row.getBytes("buyer_owner_revision")),
                row.getString("escrow_reference"), row.getString("escrow_proof"), row.getLong("pay_amount"),
                row.getInt("fee_bps"), row.getLong("cooling_ms"), row.getString("title")), orderId);
        return rows.isEmpty() ? null : rows.getFirst();
    }
    String orderByRequest(String account, String requestId) {
        var rows = jdbc.queryForList("SELECT order_sn FROM commerce_consignment_order WHERE buyer_account=? AND request_id=?",
            String.class, bytes(account), bytes(requestId));
        return rows.isEmpty() ? null : rows.getFirst();
    }
    void sameOrder(String orderId, RmbConsignmentApi.Buy input) {
        var original = mapping(orderId);
        if (original == null || !original.listingId().equals(input.listingId()) || !original.buyerAccount().equals(input.buyerAccount())
                || !original.buyerOwnerRevision().equals(input.buyerOwnerRevision())) {
            throw new CommerceException(409, "请求号已用于不同寄售或账号");
        }
    }
    boolean cancelled(String orderId) {
        var original = mapping(orderId);
        if (original == null) { return false; }
        var value = requireListing(original.listingId());
        requireCurrentOrder(value, orderId, "LOCKED");
        String next = clock.millis() >= value.view().saleEndMs() ? "RETURNING" : "LISTED";
        jdbc.update("UPDATE commerce_consignment SET state=?,current_order_sn=NULL WHERE listing_id=? AND current_order_sn=?",
            next, original.listingId(), orderId);
        return true;
    }
    boolean paid(String orderId) {
        var original = mapping(orderId);
        if (original == null) { return false; }
        requireCurrentOrder(requireListing(original.listingId()), orderId, "LOCKED");
        jdbc.update("UPDATE commerce_consignment SET state='SOLD' WHERE listing_id=? AND current_order_sn=?", original.listingId(), orderId);
        return true;
    }
    void delivered(String orderId, String outcome) {
        var original = mapping(orderId);
        if (original == null) { return; }
        requireCurrentOrder(requireListing(original.listingId()), orderId, "SOLD");
        if (outcome.equals("DELIVERED")) {
            long fee = fee(original.priceFen(), original.feeBps());
            long available = Math.addExact(clock.millis(), original.coolingMs());
            lockSeller(original.sellerAccount());
            jdbc.update("INSERT INTO commerce_seller_income(order_sn,account,gross_fen,fee_fen,net_fen,available_ms) VALUES(?,?,?,?,?,?)",
                orderId, bytes(original.sellerAccount()), original.priceFen(), fee, original.priceFen() - fee, available);
        } else if (outcome.equals("REFUND_PENDING")) {
            jdbc.update("UPDATE commerce_consignment SET state='RETURNING' WHERE listing_id=?", original.listingId());
        } else if (outcome.equals("MANUAL")) {
            jdbc.update("UPDATE commerce_consignment SET state='MANUAL' WHERE listing_id=?", original.listingId());
        } else { throw invalid(); }
    }

    public RmbConsignmentApi.Deliveries deliveries(DeliveryApi.Poll input) {
        if (input == null || input.limit() < 1 || input.limit() > 100) { throw invalid(); }
        PendingScanCursor cursor = PendingScanCursor.parse(input.cursor());
        if (cursor == null) {
            var upper = jdbc.query("SELECT d.created_ms,d.order_sn FROM commerce_delivery_intent d "
                + "JOIN commerce_consignment_order c ON c.order_sn=d.order_sn WHERE d.status='PENDING' "
                + "ORDER BY d.created_ms DESC,d.order_sn DESC LIMIT 1",
                (row, index) -> new PendingScanCursor(0, "", row.getLong(1), row.getString(2)));
            if (upper.isEmpty()) { return new RmbConsignmentApi.Deliveries(List.of()); }
            cursor = upper.getFirst();
        }
        record Pending(RmbConsignmentApi.Delivery intent, long createdMs) {}
        var rows = jdbc.query("SELECT d.*,c.buyer_account,c.buyer_owner_revision,c.listing_id,c.escrow_reference,c.escrow_proof,o.pay_amount "
            + "FROM commerce_delivery_intent d JOIN commerce_consignment_order c ON c.order_sn=d.order_sn "
            + "JOIN oms_order o ON o.order_sn=d.order_sn WHERE d.status='PENDING' "
            + "AND (d.created_ms>? OR (d.created_ms=? AND d.order_sn>?)) "
            + "AND (d.created_ms<? OR (d.created_ms=? AND d.order_sn<=?)) ORDER BY d.created_ms,d.order_sn LIMIT ?",
            (row, index) -> new Pending(new RmbConsignmentApi.Delivery(row.getString("order_sn"), row.getString("buyer_id"),
                CommerceIdentity.storedAccount(row.getBytes("buyer_account")), CommerceIdentity.storedRevision(row.getBytes("buyer_owner_revision")),
                row.getString("listing_id"), row.getString("escrow_reference"),
                row.getString("escrow_proof"), row.getLong("pay_amount")), row.getLong("created_ms")),
            cursor.afterMs(), cursor.afterMs(), cursor.afterId(), cursor.upperMs(), cursor.upperMs(), cursor.upperId(), input.limit() + 1);
        var result = new java.util.ArrayList<RmbConsignmentApi.DeliveryEntry>();
        int count = Math.min(rows.size(), input.limit());
        for (int i = 0; i < count; i++) {
            var row = rows.get(i);
            result.add(new RmbConsignmentApi.DeliveryEntry(row.intent(), rows.size() <= input.limit() && i == count - 1
                ? "" : cursor.after(row.createdMs(), row.intent().orderId())));
        }
        return new RmbConsignmentApi.Deliveries(List.copyOf(result));
    }

    /** 过期LISTED在同一待退集合内，实际退回回执事务再复核，不能与购买并行取走同一资产。 */
    public RmbConsignmentApi.Returns returns(DeliveryApi.Poll input) {
        if (input == null || input.limit() < 1 || input.limit() > 100) { throw invalid(); }
        String after = "", upper = "";
        if (input.cursor() != null && !input.cursor().isEmpty()) {
            String[] fields = input.cursor().split(":", -1);
            if (fields.length != 3 || !fields[0].equals("r1")) { throw invalid(); }
            player(fields[1]); player(fields[2]);
            after = fields[1]; upper = fields[2];
            if (after.compareTo(upper) > 0) { throw invalid(); }
        }
        long now = clock.millis();
        if (upper.isEmpty()) {
            var values = jdbc.queryForList("SELECT listing_id FROM commerce_consignment WHERE state='RETURNING' "
                + "OR (state='LISTED' AND sale_end_ms<=?) ORDER BY listing_id DESC LIMIT 1", String.class, now);
            if (values.isEmpty()) { return new RmbConsignmentApi.Returns(List.of()); }
            upper = values.getFirst();
        }
        var rows = jdbc.query("SELECT * FROM commerce_consignment WHERE listing_id>? AND listing_id<=? "
            + "AND (state='RETURNING' OR (state='LISTED' AND sale_end_ms<=?)) ORDER BY listing_id LIMIT ?",
            (row, index) -> new RmbConsignmentApi.Return(row.getString("listing_id"), row.getString("seller_player_id"),
                text(row.getBytes("seller_account")), row.getString("escrow_reference"), row.getString("escrow_proof")),
            after, upper, now, input.limit() + 1);
        var entries = new java.util.ArrayList<RmbConsignmentApi.ReturnEntry>();
        int count = Math.min(rows.size(), input.limit());
        for (int i = 0; i < count; i++) {
            entries.add(new RmbConsignmentApi.ReturnEntry(rows.get(i), rows.size() <= input.limit() && i == count - 1
                ? "" : "r1:" + rows.get(i).listingId() + ":" + upper));
        }
        return new RmbConsignmentApi.Returns(List.copyOf(entries));
    }
    public void returned(RmbConsignmentApi.ReturnReceipt input) {
        if (input == null) { throw invalid(); }
        player(input.listingId()); player(input.escrowReference()); proof(input.escrowProof()); proof(input.returnProof());
        tx.executeWithoutResult(status -> {
            var original = requireListing(input.listingId());
            if (!original.escrowReference().equals(input.escrowReference()) || !original.escrowProof().equals(input.escrowProof())) {
                throw new CommerceException(409, "退回资产原合同不符");
            }
            var prior = jdbc.queryForList("SELECT return_proof FROM commerce_consignment_return WHERE listing_id=?", String.class, input.listingId());
            if (!prior.isEmpty()) {
                if (!prior.getFirst().equals(input.returnProof())) { throw new CommerceException(409, "退回终局原件冲突"); }
                return;
            }
            if (!original.view().state().equals("RETURNING") && !(original.view().state().equals("LISTED")
                    && clock.millis() >= original.view().saleEndMs())) {
                throw new CommerceException(409, "寄售不在待退回状态");
            }
            jdbc.update("INSERT INTO commerce_consignment_return(listing_id,return_proof) VALUES(?,?)", input.listingId(), input.returnProof());
            jdbc.update("UPDATE commerce_consignment SET state='RETURNED' WHERE listing_id=?", input.listingId());
        });
    }

    public RmbConsignmentApi.Incomes incomes(RmbConsignmentApi.IncomePage page) {
        if (page == null || page.afterOrderId() == null || page.limit() < 1 || page.limit() > 100) { throw invalid(); }
        account(page.account());
        if (!page.afterOrderId().isEmpty()) { uuid(page.afterOrderId()); }
        return new RmbConsignmentApi.Incomes(jdbc.query("SELECT * FROM commerce_seller_income WHERE account=? AND order_sn>? "
            + "ORDER BY order_sn LIMIT ?", (row, index) -> income(row), bytes(page.account()), page.afterOrderId(), page.limit()));
    }

    public RmbConsignmentApi.Withdrawal withdraw(RmbConsignmentApi.Withdraw input) {
        if (input == null || input.amountFen() < 1 || input.amountFen() > CommerceStore.MAX_AMOUNT_MINOR) { throw invalid(); }
        account(input.account()); request(input.requestId());
        return Objects.requireNonNull(tx.execute(status -> {
            lockSeller(input.account());
            var prior = jdbc.queryForList("SELECT withdrawal_id,amount_fen FROM commerce_withdrawal WHERE account=? AND request_id=?",
                bytes(input.account()), bytes(input.requestId()));
            if (!prior.isEmpty()) {
                if (((Number) prior.getFirst().get("amount_fen")).longValue() != input.amountFen()) {
                    throw new CommerceException(409, "提现请求号已用于不同金额");
                }
                return withdrawal((String) prior.getFirst().get("withdrawal_id"), input.account(), false);
            }
            var funds = jdbc.query("SELECT * FROM commerce_seller_income WHERE account=? AND available_ms<=? "
                + "AND net_fen-reserved_fen-paid_fen-forfeited_fen>0 ORDER BY order_sn LIMIT 100 FOR UPDATE",
                (row, index) -> income(row), bytes(input.account()), clock.millis());
            long remaining = input.amountFen();
            for (var row : funds) { remaining -= Math.min(remaining, row.netFen() - row.reservedFen() - row.paidFen() - row.forfeitedFen()); }
            if (remaining != 0) { throw new CommerceException(409, "可提现收入不足或本次分配超过100单上限"); }
            String id = UUID.randomUUID().toString();
            jdbc.update("INSERT INTO commerce_withdrawal(withdrawal_id,account,request_id,amount_fen,state,created_ms)"
                + " VALUES(?,?,?,?,'REQUESTED',?)", id, bytes(input.account()), bytes(input.requestId()), input.amountFen(), clock.millis());
            remaining = input.amountFen();
            for (var row : funds) {
                long amount = Math.min(remaining, row.netFen() - row.reservedFen() - row.paidFen() - row.forfeitedFen());
                if (amount == 0) { break; }
                if (jdbc.update("UPDATE commerce_seller_income SET reserved_fen=reserved_fen+? WHERE order_sn=? "
                        + "AND net_fen-reserved_fen-paid_fen-forfeited_fen>=?", amount, row.orderId(), amount) != 1) {
                    throw new CommerceException(503, "收入预留并发状态异常");
                }
                jdbc.update("INSERT INTO commerce_withdrawal_allocation(withdrawal_id,order_sn,amount_fen) VALUES(?,?,?)",
                    id, row.orderId(), amount);
                remaining -= amount;
            }
            return withdrawal(id, input.account(), false);
        }));
    }

    public RmbConsignmentApi.Withdrawal getWithdrawal(RmbConsignmentApi.WithdrawalQuery input) {
        if (input == null) { throw invalid(); }
        uuid(input.withdrawalId()); account(input.account());
        return withdrawal(input.withdrawalId(), input.account(), false);
    }
    public RmbConsignmentApi.Withdrawal completeWithdrawal(RmbConsignmentApi.WithdrawalReceipt input) {
        if (input == null || input.amountFen() < 1 || !"CNY".equals(input.currency()) || input.channel() == null
            || !input.channel().matches("[a-z][a-z0-9_-]{1,31}") || input.transactionId() == null
            || !input.transactionId().matches("[A-Za-z0-9_-]{1,128}")) { throw invalid(); }
        uuid(input.withdrawalId());
        if (payouts == null) { throw new CommerceException(503, "出款事实验证器未装配"); }
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                var accounts = jdbc.query("SELECT account FROM commerce_withdrawal WHERE withdrawal_id=?",
                    (row, index) -> text(row.getBytes(1)), input.withdrawalId());
                if (accounts.isEmpty()) { throw new CommerceException(404, "提现单不存在"); }
                String owner = accounts.getFirst();
                lockSeller(owner);
                var current = withdrawal(input.withdrawalId(), owner, true);
                if (current.amountFen() != input.amountFen()) { throw new CommerceException(409, "出款金额不符"); }
                payouts.verify(jdbc, input);
                var prior = jdbc.queryForList("SELECT * FROM commerce_withdrawal_receipt WHERE withdrawal_id=?", input.withdrawalId());
                if (!prior.isEmpty()) {
                    var row = prior.getFirst();
                    if (!Arrays.equals(bytes(input.channel()), (byte[]) row.get("channel"))
                        || !Arrays.equals(bytes(input.transactionId()), (byte[]) row.get("transaction_id"))
                        || ((Number) row.get("successful")).intValue() != (input.successful() ? 1 : 0)) {
                        throw new CommerceException(409, "出款终局回执冲突");
                    }
                    return current;
                }
                if (!current.state().equals("REQUESTED")) { throw new CommerceException(409, "提现单不在待出款状态"); }
                var allocations = jdbc.queryForList("SELECT order_sn,amount_fen FROM commerce_withdrawal_allocation "
                    + "WHERE withdrawal_id=? ORDER BY order_sn", input.withdrawalId());
                long allocated = 0;
                for (var row : allocations) {
                    long amount = ((Number) row.get("amount_fen")).longValue();
                    if (amount <= 0 || amount > input.amountFen() - allocated) { throw new CommerceException(503, "出款分配原件损坏"); }
                    allocated += amount;
                    int changed = jdbc.update("UPDATE commerce_seller_income SET reserved_fen=reserved_fen-?,paid_fen=paid_fen+? "
                        + "WHERE order_sn=? AND account=? AND reserved_fen>=?", amount, input.successful() ? amount : 0,
                        row.get("order_sn"), bytes(owner), amount);
                    if (changed != 1) { throw new CommerceException(503, "收入预留原件不符"); }
                }
                if (allocated != input.amountFen()) { throw new CommerceException(503, "出款分配总额不符"); }
                jdbc.update("INSERT INTO commerce_withdrawal_receipt(withdrawal_id,channel,transaction_id,amount_fen,successful) VALUES(?,?,?,?,?)",
                    input.withdrawalId(), bytes(input.channel()), bytes(input.transactionId()), input.amountFen(), input.successful() ? 1 : 0);
                jdbc.update("UPDATE commerce_withdrawal SET state=?,channel=?,transaction_id=? WHERE withdrawal_id=? AND state='REQUESTED'",
                    input.successful() ? "PAID" : "FAILED", input.channel(), input.transactionId(), input.withdrawalId());
                return withdrawal(input.withdrawalId(), owner, false);
            }));
        } catch (DuplicateKeyException conflict) { throw new CommerceException(409, "出款流水已用于其它提现"); }
    }

    static long fee(long gross, int bps) {
        if (gross < 1 || gross > CommerceStore.MAX_AMOUNT_MINOR || bps < 0 || bps > 10_000) { throw invalid(); }
        return (gross / 10_000) * bps + ((gross % 10_000) * bps + 9_999) / 10_000;
    }
    private RmbConsignmentApi.Withdrawal withdrawal(String id, String owner, boolean lock) {
        var rows = jdbc.query("SELECT * FROM commerce_withdrawal WHERE withdrawal_id=? AND account=?" + (lock ? " FOR UPDATE" : ""),
            (row, index) -> new RmbConsignmentApi.Withdrawal(row.getString("withdrawal_id"), row.getLong("amount_fen"),
                row.getString("state"), row.getString("channel"), row.getString("transaction_id")), id, bytes(owner));
        if (rows.isEmpty()) { throw new CommerceException(404, "提现单不存在"); }
        return rows.getFirst();
    }
    private void lockSeller(String owner) {
        jdbc.update("INSERT INTO commerce_seller_guard(account) VALUES(?) ON DUPLICATE KEY UPDATE account=account", bytes(owner));
        jdbc.queryForObject("SELECT account FROM commerce_seller_guard WHERE account=? FOR UPDATE", byte[].class, bytes(owner));
    }
    private Stored requireListing(String id) {
        var values = listing(id, true);
        if (values.isEmpty()) { throw new CommerceException(404, "寄售不存在"); }
        return values.getFirst();
    }
    private List<Stored> listing(String id, boolean lock) {
        return jdbc.query("SELECT * FROM commerce_consignment WHERE listing_id=?" + (lock ? " FOR UPDATE" : ""),
            (row, index) -> stored(row), id);
    }
    private Stored stored(ResultSet row) throws SQLException {
        String state = row.getString("state");
        long notice = row.getLong("notice_end_ms"), end = row.getLong("sale_end_ms"), now = clock.millis();
        String phase = state.equals("LISTED") ? (now < notice ? "PUBLIC_NOTICE" : now < end ? "ON_SALE" : "ENDED") : state;
        var view = new RmbConsignmentApi.Listing(row.getString("listing_id"), row.getString("seller_player_id"), row.getLong("market_zone"),
            row.getString("asset_kind"), row.getString("title"), row.getLong("price_fen"), row.getInt("fee_bps"),
            row.getLong("cooling_ms"), notice, end, state, phase, row.getString("current_order_sn"));
        return new Stored(view, text(row.getBytes("seller_account")), row.getString("escrow_reference"),
            row.getString("escrow_proof"), row.getString("request_digest"));
    }
    private static RmbConsignmentApi.Income income(ResultSet row) throws SQLException {
        return new RmbConsignmentApi.Income(row.getString("order_sn"), row.getLong("gross_fen"), row.getLong("fee_fen"),
            row.getLong("net_fen"), row.getLong("reserved_fen"), row.getLong("paid_fen"), row.getLong("available_ms"),
            row.getLong("forfeited_fen"));
    }
    private static void requireCurrentOrder(Stored value, String orderId, String state) {
        if (!state.equals(value.view().state()) || !orderId.equals(value.view().orderId())) {
            throw new CommerceException(503, "寄售当前订单原件与OMS不符");
        }
    }
    private String digest(Object input) {
        try { return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(json.writeValueAsBytes(input))); }
        catch (NoSuchAlgorithmException | JsonProcessingException error) { throw new IllegalStateException("寄售摘要不可用", error); }
    }
    private static byte[] bytes(String value) { return value.getBytes(StandardCharsets.UTF_8); }
    private static String text(byte[] value) { return new String(value, StandardCharsets.UTF_8); }
    static void account(String value) {
        CommerceIdentity.account(value);
    }
    static void player(String value) {
        try { HmacAuthentication.requirePlayer(value); }
        catch (CommerceException invalidPlayer) { throw invalid(); }
    }
    static void request(String value) { if (value == null || !value.matches("[A-Za-z0-9_-]{8,128}")) { throw invalid(); } }
    private static void proof(String value) { if (value == null || !value.matches("[0-9a-f]{64}")) { throw invalid(); } }
    private static void uuid(String value) {
        if (value == null || !value.matches("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")) { throw invalid(); }
    }
    private static CommerceException invalid() { return new CommerceException(400, "人民币寄售或结算参数非法"); }
}
