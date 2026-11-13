package com.game.mall;

import com.macro.mall.model.OmsOrder;
import com.macro.mall.model.PmsSkuStock;
import java.math.BigDecimal;
import java.nio.charset.StandardCharsets;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Clock;
import java.util.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.context.annotation.DependsOn;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;

@Repository
@DependsOn("commerceSchemaReady")
public class CommerceStore {
    private static final Logger LOG = LoggerFactory.getLogger(CommerceStore.class);
    static final long MAX_AMOUNT_MINOR = 9_000_000_000_000_000L;
    /** 已付过的订单状态(待交付、已交付、退款中、已退款、人工、扣回中、已扣回):再来的付款不再绑定订单。 */
    private static final Set<Integer> PAID_STATES = Set.of(1, 2, 5, 6, 7, 8, 9);
    private final JdbcTemplate jdbc;
    private final TransactionTemplate tx;
    private final Clock clock;
    private final int maxUnpaid;
    private final PaymentPolicy paymentPolicy;
    private final RmbConsignmentStore consignments;
    public CommerceStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock, CommerceProperties props) {
        this(jdbc, manager, clock, props, new RmbConsignmentStore(jdbc, manager, clock,
            new RmbConsignmentProperties(), (RmbConsignmentStore.PayoutEvidenceVerifier) null));
    }
    @Autowired public CommerceStore(JdbcTemplate jdbc, PlatformTransactionManager manager, Clock clock,
            CommerceProperties props, RmbConsignmentStore consignments) {
        this.jdbc = jdbc; this.clock = clock;
        this.consignments = consignments;
        maxUnpaid = props.getMaxUnpaidOrders();
        paymentPolicy = new PaymentPolicy(jdbc, props);
        tx = new TransactionTemplate(manager); tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED); tx.setTimeout(5);
        jdbc.setQueryTimeout(5);
    }

    public CommerceApi.Order create(String buyer, CommerceApi.CreateOrder in) {
        HmacAuthentication.requirePlayer(buyer);
        if (in == null || in.requestId() == null || !in.requestId().matches("[A-Za-z0-9_-]{8,128}") || in.skuId() <= 0 || in.quantity() < 1 || in.quantity() > 10000) {
            throw new CommerceException(400, "下单参数非法");
        }
        CommerceIdentity.account(in.buyerAccount()); CommerceIdentity.revision(in.buyerOwnerRevision());
        Stored prior = byRequest(buyer, in.requestId());
        if (prior != null) { return sameRequest(prior, in); }
        // 首次建 guard 为单条独立提交，行从不删除；没有全服共享锁行。
        jdbc.update("INSERT INTO commerce_buyer_guard(buyer_id) VALUES (?) ON DUPLICATE KEY UPDATE buyer_id=buyer_id", buyer);
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                jdbc.queryForObject("SELECT buyer_id FROM commerce_buyer_guard WHERE buyer_id=? FOR UPDATE", String.class, buyer);
                Stored replay = byRequest(buyer, in.requestId());
                if (replay != null) { return sameRequest(replay, in); }
                Integer unpaid = jdbc.queryForObject("SELECT COUNT(*) FROM oms_order WHERE buyer_id=? AND status=0", Integer.class, buyer);
                if (unpaid == null || unpaid >= maxUnpaid) { throw new CommerceException(429, "未支付订单数量已达上限，请先处理已有订单"); }
                List<Sku> values = jdbc.query("SELECT * FROM pms_sku_stock WHERE id=? FOR UPDATE", (rs, row) -> sku(rs), in.skuId());
                if (values.isEmpty()) { throw new CommerceException(404, "商品不存在"); }
                Sku sku = values.get(0);
                // 同 SKU 竞争者可能刚提交相同请求；当前读隔离下重新复核，原价订单不会被新价格覆盖。
                Stored existing = byRequest(buyer, in.requestId());
                if (existing != null) { return sameRequest(existing, in); }
                PmsSkuStock model = sku.model();
                if (!sku.published() || model.getStock() < 0 || model.getLockStock() < 0 || model.getLockStock() > model.getStock()) {
                    throw new CommerceException(409, "商品未开放或库存状态异常");
                }
                long unitPrice = model.getPrice().longValueExact();
                long total;
                try { total = Math.multiplyExact(unitPrice, in.quantity()); }
                catch (ArithmeticException overflow) { throw new CommerceException(409, "商品金额超限"); }
                if (unitPrice <= 0 || total <= 0 || total > MAX_AMOUNT_MINOR || !sku.currency().matches("[A-Z][A-Z0-9_]{2,15}")) {
                    throw new CommerceException(409, "商品金额或币种非法");
                }
                String orderId = UUID.randomUUID().toString();
                long now = clock.millis();
                // 15 分钟支付窗口供渠道适配器使用；超时仍保留预占，未接关单确认前不自动释放。
                jdbc.update("INSERT INTO oms_order(order_sn,buyer_id,request_id,sku_id,quantity,pay_amount,currency,status,create_time_ms,payment_deadline_ms) VALUES (?,?,?,?,?,?,?,0,?,?)",
                        orderId, buyer, ascii(in.requestId()), in.skuId(), in.quantity(), total, sku.currency(), now, now + 900_000);
                jdbc.update("INSERT INTO commerce_official_owner(order_sn,buyer_account,buyer_owner_revision) VALUES(?,?,?)",
                    orderId, in.buyerAccount().getBytes(StandardCharsets.UTF_8), ascii(in.buyerOwnerRevision()));
                // 改自上游库存双计数：持主键行锁，预占条件在写入时原子复核，绝不先读后绝对值覆盖。
                int changed = jdbc.update("UPDATE pms_sku_stock SET lock_stock=lock_stock+? WHERE id=? AND published=1 AND stock-lock_stock>=?",
                        in.quantity(), in.skuId(), in.quantity());
                if (changed != 1) { throw new CommerceException(409, "库存不足"); }
                jdbc.update("INSERT INTO oms_order_item(order_sn,product_id,product_sku_id,product_sku_code,product_quantity,product_price_minor) VALUES (?,?,?,?,?,?)",
                        orderId, model.getProductId(), in.skuId(), model.getSkuCode(), in.quantity(), unitPrice);
                // 发放物二选一(物品或货币)冻结进订单;物品一单至多 4096 件,货币一单至多 DeliveryStore.MAX_CURRENCY_GRANT
                // (商城金额上限的同一量级)。货币乘数量先转 DECIMAL 再比:BIGINT 相乘越界在 MySQL / TiDB 是报错而不是假。
                if(jdbc.update("INSERT INTO commerce_order_grant(order_sn,item_config_id,count_per_unit,currency_type,currency_amount,title) "
                    + "SELECT ?,item_config_id,count_per_unit,currency_type,currency_amount,title FROM commerce_sku_grant WHERE sku_id=? AND CHAR_LENGTH(TRIM(title))>0 AND ("
                    + "(item_config_id>0 AND count_per_unit>0 AND count_per_unit*?<=4096 AND currency_type=0 AND currency_amount=0) OR "
                    + "(item_config_id=0 AND count_per_unit=0 AND currency_type>=0 AND currency_amount>0 AND CAST(currency_amount AS DECIMAL(30,0))*?<=?))",
                    orderId,in.skuId(),in.quantity(),in.quantity(),DeliveryStore.MAX_CURRENCY_GRANT)!=1)throw new CommerceException(409,"SKU没有合法游戏交付映射或单次数量越界");
                return view(Objects.requireNonNull(byId(orderId, false)));
            }));
        } catch (DuplicateKeyException conflict) {
            // 跨 SKU 的同幂等键竞争由唯一约束裁决；失败事务先回滚，再返回已提交的同一结果。
            Stored existing = byRequest(buyer, in.requestId());
            if (existing != null) { return sameRequest(existing, in); }
            throw new CommerceException(409, "请求冲突，请使用原请求号重试");
        }
    }

    public CommerceApi.Order get(String buyer, String orderId) {
        HmacAuthentication.requirePlayer(buyer); requireOrderId(orderId);
        throw new CommerceException(404, "订单查询需要原账号和归属修订");
    }
    public CommerceApi.Order get(String buyer, CommerceApi.OwnedOrder input) {
        HmacAuthentication.requirePlayer(buyer);
        return view(officialOwned(input, buyer, false));
    }

    public CommerceApi.Order createConsignment(RmbConsignmentApi.Buy input) {
        if (input == null) { throw new CommerceException(400, "寄售购买参数缺失"); }
        RmbConsignmentStore.account(input.buyerAccount()); RmbConsignmentStore.player(input.buyerPlayerId());
        CommerceIdentity.revision(input.buyerOwnerRevision());
        RmbConsignmentStore.player(input.listingId()); RmbConsignmentStore.request(input.requestId());
        String prior = consignments.orderByRequest(input.buyerAccount(), input.requestId());
        if (prior != null) { return sameConsignment(prior, input); }
        jdbc.update("INSERT INTO commerce_buyer_guard(buyer_id) VALUES(?) ON DUPLICATE KEY UPDATE buyer_id=buyer_id", input.buyerPlayerId());
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                jdbc.queryForObject("SELECT buyer_id FROM commerce_buyer_guard WHERE buyer_id=? FOR UPDATE", String.class, input.buyerPlayerId());
                String replay = consignments.orderByRequest(input.buyerAccount(), input.requestId());
                if (replay != null) { return sameConsignment(replay, input); }
                if (byRequest(input.buyerPlayerId(), input.requestId()) != null) {
                    throw new CommerceException(409, "请求号已用于其它订单");
                }
                Integer unpaid = jdbc.queryForObject("SELECT COUNT(*) FROM oms_order WHERE buyer_id=? AND status=0", Integer.class, input.buyerPlayerId());
                if (unpaid == null || unpaid >= maxUnpaid) { throw new CommerceException(429, "未支付订单数量已达上限"); }
                String orderId = UUID.randomUUID().toString();
                var frozen = consignments.reserve(input, orderId);
                long now = clock.millis();
                // 寄售仍写同一OMS，sku_id=0明确区分唯一托管资产，不改PMS库存。
                jdbc.update("INSERT INTO oms_order(order_sn,buyer_id,request_id,sku_id,quantity,pay_amount,currency,status,create_time_ms,payment_deadline_ms)"
                    + " VALUES(?,?,?,0,1,?,'CNY',0,?,?)", orderId, input.buyerPlayerId(), ascii(input.requestId()), frozen.priceFen(), now, now + 900_000);
                jdbc.update("INSERT INTO oms_order_item(order_sn,product_id,product_sku_id,product_sku_code,product_quantity,product_price_minor)"
                    + " VALUES(?,0,0,?,1,?)", orderId, "consignment-" + frozen.listingId(), frozen.priceFen());
                return view(Objects.requireNonNull(byId(orderId, false)));
            }));
        } catch (DuplicateKeyException conflict) {
            String existing = consignments.orderByRequest(input.buyerAccount(), input.requestId());
            if (existing != null) { return sameConsignment(existing, input); }
            throw new CommerceException(409, "寄售购买请求冲突，请使用原请求号重试");
        }
    }
    private CommerceApi.Order sameConsignment(String orderId, RmbConsignmentApi.Buy input) {
        consignments.sameOrder(orderId, input);
        Stored original = byId(orderId, false);
        if (original == null || !original.buyer().equals(input.buyerPlayerId())) {
            throw new CommerceException(409, "购买请求号的角色原件不符");
        }
        return view(original);
    }
    public CommerceApi.Order getConsignment(RmbConsignmentApi.OrderQuery input) {
        if (input == null) { throw new CommerceException(400, "寄售订单查询参数缺失"); }
        return view(consignmentOwned(input, false));
    }
    public List<CommerceApi.Order> listConsignments(RmbConsignmentApi.OrderPage input) {
        if (input == null || input.afterOrderId() == null || input.limit() < 1 || input.limit() > 100) {
            throw new CommerceException(400, "寄售订单分页非法");
        }
        RmbConsignmentStore.account(input.account());
        long after = input.afterOrderId().isEmpty() ? 0 : consignmentOwned(new RmbConsignmentApi.OrderQuery(input.account(), input.afterOrderId()), false).model().getId();
        return jdbc.query("SELECT o.* FROM oms_order o JOIN commerce_consignment_order c ON c.order_sn=o.order_sn "
            + "WHERE c.buyer_account=? AND o.id>? ORDER BY o.id LIMIT ?", (row, index) -> view(stored(row)),
            input.account().getBytes(StandardCharsets.UTF_8), after, input.limit());
    }
    private Stored consignmentOwned(RmbConsignmentApi.OrderQuery input, boolean lock) {
        RmbConsignmentStore.account(input.account()); requireOrderId(input.orderId());
        Stored original = byId(input.orderId(), lock);
        var mapping = consignments.mapping(input.orderId());
        if (original == null || mapping == null || !mapping.buyerAccount().equals(input.account())) {
            throw new CommerceException(404, "寄售订单不存在");
        }
        return original;
    }
    // 只供内部payment/admin只读控制器使用，均再次核角色；不接受外部买家或金额。
    CommerceApi.Order paymentOrderView(String orderId) {
        requireOrderId(orderId);
        Stored order = byId(orderId, false);
        if (order == null) { throw new CommerceException(404, "订单不存在"); }
        return view(order);
    }
    public List<CommerceApi.Order> list(String buyer,DeliveryApi.OrderPage page){
        HmacAuthentication.requirePlayer(buyer);if(page==null||page.limit()<1||page.limit()>100||page.afterOrderId()==null)throw new CommerceException(400,"订单分页非法");
        CommerceIdentity.account(page.buyerAccount()); CommerceIdentity.revision(page.buyerOwnerRevision());
        long after=0;if(!page.afterOrderId().isEmpty()){after=officialOwned(new CommerceApi.OwnedOrder(page.afterOrderId(),page.buyerAccount(),page.buyerOwnerRevision()),buyer,false).model().getId();}
        return jdbc.query("SELECT o.* FROM oms_order o JOIN commerce_official_owner p ON p.order_sn=o.order_sn "
            + "WHERE o.buyer_id=? AND p.buyer_account=? AND p.buyer_owner_revision=? AND o.sku_id<>0 AND o.id>? ORDER BY o.id LIMIT ?",
            (row,n)->view(stored(row)),buyer,page.buyerAccount().getBytes(StandardCharsets.UTF_8),ascii(page.buyerOwnerRevision()),after,page.limit());
    }

    public CommerceApi.Order cancel(String buyer, String orderId) {
        HmacAuthentication.requirePlayer(buyer); requireOrderId(orderId);
        throw new CommerceException(404, "订单取消需要原账号和归属修订");
    }
    public CommerceApi.Order cancel(String buyer, CommerceApi.OwnedOrder input) {
        HmacAuthentication.requirePlayer(buyer);
        return Objects.requireNonNull(tx.execute(status -> {
            return cancelOrder(officialOwned(input, buyer, true));
        }));
    }
    public CommerceApi.Order cancelConsignment(RmbConsignmentApi.OrderQuery input) {
        if (input == null) { throw new CommerceException(400, "寄售订单取消参数缺失"); }
        return Objects.requireNonNull(tx.execute(status -> cancelOrder(consignmentOwned(input, true))));
    }
    private CommerceApi.Order cancelOrder(Stored order) {
            String orderId = order.model().getOrderSn();
            if (order.model().getStatus() == 4) { return view(order); }
            if (order.model().getStatus() != 0) { throw new CommerceException(409, "已支付订单不能直接取消"); }
            if (order.skuId() == 0) {
                if (!consignments.cancelled(orderId)) { throw new CommerceException(503, "寄售订单关联缺失"); }
            } else {
                int released = jdbc.update("UPDATE pms_sku_stock SET lock_stock=lock_stock-? WHERE id=? AND lock_stock>=?",
                    order.quantity(), order.skuId(), order.quantity());
                if (released != 1) { throw new CommerceException(409, "库存预占状态异常"); }
            }
            int changed = jdbc.update("UPDATE oms_order SET status=4 WHERE order_sn=? AND status=0", orderId);
            if (changed != 1) { throw new CommerceException(409, "订单状态冲突"); }
            order.model().setStatus(4); return view(order);
    }

    /**
     * 记一笔可信适配器报来的付款。每笔付款在 commerce_payment_receipt 占一行,交易键 (channel, transaction_id) 全局唯一,
     * 由库的主键保证一笔交易只记一次。按顺序判定这笔钱的去向,每种都在同一事务里写完;适配器从应答的 refundReason 读出结论
     * (见 CommerceApi.PaymentOutcome):
     * <ol>
     *   <li>交易键已记过:重放,原样回已记下的结论,不写任何东西(见 replay)。</li>
     *   <li>币种或金额与订单冻结值不符:异常付款 CURRENCY_MISMATCH / AMOUNT_MISMATCH。先于订单状态判定:
     *       对不上的钱在任何状态下都不能算作这张订单的付款。</li>
     *   <li>订单已付过:异常付款 DUPLICATE_PAYMENT。</li>
     *   <li>订单已取消:这笔绑定原单,订单 4→5,退款待办 CANCELLED_LATE。</li>
     *   <li>订单待付款:这笔绑定订单,扣库存、写交付意图,订单 0→1。</li>
     * </ol>
     * 并发:同一订单的回调在订单行锁上排队,后到者看见先到者已提交的回执,走重放;报同一交易键的两张订单各持各的行锁,
     * 在回执主键上撞键,先提交的成功,另一个整笔回滚并回 409。
     * <p>409 只表示适配器报来的这笔与已记下的冲突(交易键已属别的订单,或改了金额币种),适配器可以当终局。存储不变量被破坏
     * (库存预占、订单状态、已记录的几行互相对不上、撞上别的唯一键)一律回 503:这笔钱没有记下,适配器必须保留它、退避重试并告警,
     * 不能当终局放掉。
     */
    public CommerceApi.PaymentOutcome recordTrustedPayment(CommerceApi.Payment in) {
        requireOrderId(in.orderId());
        if (in.channel() == null || !in.channel().matches("[a-z][a-z0-9_-]{1,31}") || in.transactionId() == null || !in.transactionId().matches("[A-Za-z0-9_-]{1,128}") || in.amountMinor() <= 0 || in.amountMinor() > MAX_AMOUNT_MINOR || in.currency() == null || !in.currency().matches("[A-Z][A-Z0-9_]{2,15}")) {
            throw new CommerceException(400, "支付回执参数非法");
        }
        try {
            return Objects.requireNonNull(tx.execute(status -> {
                Stored order = byId(in.orderId(), true);
                if (order == null) { throw new CommerceException(404, "订单不存在"); }
                if (order.skuId() != 0) { officialOwner(in.orderId()); }
                paymentPolicy.payment(in);
                KnownPayment known = knownPayment(in);
                if (known != null) { return replay(order, in, known); }
                CommerceApi.RefundReason mismatch = mismatch(order, in);
                if (mismatch != null) { return abnormalPayment(order, in, mismatch); }
                int state = order.model().getStatus();
                if (PAID_STATES.contains(state)) { return abnormalPayment(order, in, CommerceApi.RefundReason.DUPLICATE_PAYMENT); }
                if (state == 4) { return latePayment(order, in); }
                if (state != 0) { throw new CommerceException(503, "订单状态无法处理"); }
                return firstPayment(order, in);
            }));
        } catch (DuplicateKeyException conflict) { throw duplicateKeyConflict(in); }
    }

    /**
     * 写入撞了唯一键,事务已整笔回滚。回滚后重读这个交易键:它确实已记在另一张订单名下(两张订单并发报同一笔,先提交的那张赢了),
     * 才是适配器的错,回 409;读不到、记录不自洽、或者就记在本单名下,说明撞的是别的唯一键(孤儿待办、本单已有绑定回执之类),
     * 存储已经坏了,回 503。
     */
    private CommerceException duplicateKeyConflict(CommerceApi.Payment in) {
        KnownPayment owner = knownPayment(in);
        if (owner != null && owner.consistent() && !in.orderId().equals(owner.order())) {
            return new CommerceException(409, "支付流水已用于其他订单");
        }
        LOG.error("payment insert hit an unexpected unique key, storage invariant broken: order={} channel={}", in.orderId(), in.channel());
        return new CommerceException(503, "付款记录撞上未预期的唯一约束");
    }

    /** 付款与订单冻结的币种、金额比较,都相符返回 null。币种不同就不再比金额:不同币种的金额没有可比性。 */
    private static CommerceApi.RefundReason mismatch(Stored order, CommerceApi.Payment in) {
        if (!order.currency().equals(in.currency())) { return CommerceApi.RefundReason.CURRENCY_MISMATCH; }
        if (order.model().getPayAmount().longValueExact() != in.amountMinor()) { return CommerceApi.RefundReason.AMOUNT_MISMATCH; }
        return null;
    }

    /** 订单待付款且金额币种相符:这笔绑定订单。 */
    private CommerceApi.PaymentOutcome firstPayment(Stored order, CommerceApi.Payment in) {
        jdbc.update("INSERT INTO commerce_payment_receipt(channel,transaction_id,order_sn,amount_minor,currency) VALUES (?,?,?,?,?)",
                ascii(in.channel()), ascii(in.transactionId()), in.orderId(), in.amountMinor(), in.currency());
        // 复用上游 stock/lock_stock 一并扣减结构；补足状态 CAS、幂等收据及非负条件。
        // 下单时已预占、又持着订单行锁，下面两处只会因存储损坏失败：回 503，不回 409(见 recordTrustedPayment)。
        if (order.skuId() == 0) {
            if (!consignments.paid(in.orderId())) { throw new CommerceException(503, "寄售订单关联缺失"); }
        } else {
            int deducted = jdbc.update("UPDATE pms_sku_stock SET stock=stock-?,lock_stock=lock_stock-? WHERE id=? AND stock>=? AND lock_stock>=?",
                order.quantity(), order.quantity(), order.skuId(), order.quantity(), order.quantity());
            if (deducted != 1) { throw new CommerceException(503, "库存预占状态异常"); }
        }
        int changed = jdbc.update("UPDATE oms_order SET status=1,payment_time_ms=? WHERE order_sn=? AND status=0", clock.millis(), in.orderId());
        if (changed != 1) { throw new CommerceException(503, "订单状态冲突"); }
        jdbc.update("INSERT INTO commerce_delivery_intent(order_sn,buyer_id,sku_id,quantity,status,created_ms) VALUES (?,?,?,?,?,?)",
                in.orderId(), order.buyer(), order.skuId(), order.quantity(), "PENDING", clock.millis());
        // 此提交只确认支付和交付意图；没有可信游戏资产持久回执，永不迁移到完成。
        order.model().setStatus(1);
        return new CommerceApi.PaymentOutcome(view(order), null);
    }

    /** 订单取消后才到账且金额币种相符:这笔绑定原单,随订单一起退,订单 4→5。 */
    private CommerceApi.PaymentOutcome latePayment(Stored order, CommerceApi.Payment in) {
        jdbc.update("INSERT INTO commerce_payment_receipt(channel,transaction_id,order_sn,amount_minor,currency) VALUES(?,?,?,?,?)",
            ascii(in.channel()), ascii(in.transactionId()), in.orderId(), in.amountMinor(), in.currency());
        queueRefund(in, CommerceApi.RefundReason.CANCELLED_LATE);
        jdbc.update("UPDATE oms_order SET status=5 WHERE order_sn=? AND status=4", in.orderId());
        order.model().setStatus(5);
        return new CommerceApi.PaymentOutcome(view(order), CommerceApi.RefundReason.CANCELLED_LATE);
    }

    /**
     * 异常付款:钱已经进了渠道,却不能算作这张订单的付款 —— 订单已由另一笔付清(第一轮数据审计 §4 第 19 条,2026-11-05),
     * 或金额币种与订单冻结值不符(第二轮数据审计评审 R3,2026-11-10)。旧实现回 409 了事,系统里没有任何记录,也没有退款路径。
     * 现在同一事务里写三行:一行不绑定订单的付款回执(order_sn 为 NULL,与绑定的回执共用交易键主键)、一行异常付款记录
     * (冲的是哪张订单、为什么没绑定)、一条同原因的退款待办;订单状态、库存、交付意图都不动。
     */
    private CommerceApi.PaymentOutcome abnormalPayment(Stored order, CommerceApi.Payment in, CommerceApi.RefundReason reason) {
        jdbc.update("INSERT INTO commerce_payment_receipt(channel,transaction_id,order_sn,amount_minor,currency) VALUES(?,?,NULL,?,?)",
            ascii(in.channel()), ascii(in.transactionId()), in.amountMinor(), in.currency());
        jdbc.update("INSERT INTO commerce_abnormal_payment(channel,transaction_id,order_sn,reason,created_ms) VALUES(?,?,?,?,?)",
            ascii(in.channel()), ascii(in.transactionId()), in.orderId(), reason.name(), clock.millis());
        queueRefund(in, reason);
        // 提交成功后才记日志：事务回滚时不能留下「已记录」的假象。
        TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
            @Override public void afterCommit() {
                LOG.warn("abnormal payment recorded and queued for refund: reason={} order={} channel={}", reason, in.orderId(), in.channel());
            }
        });
        return new CommerceApi.PaymentOutcome(view(order), reason);
    }

    /** 退款待办按要退的那笔付款的交易键建,一笔付款至多一条。 */
    private void queueRefund(CommerceApi.Payment in, CommerceApi.RefundReason reason) {
        jdbc.update("INSERT INTO commerce_refund_intent(channel,transaction_id,order_sn,source,status) VALUES(?,?,?,?,'PENDING')",
            ascii(in.channel()), ascii(in.transactionId()), in.orderId(), reason.name());
    }

    /**
     * 同一交易键再次报来。一笔渠道交易只属于一张订单、只有一个金额币种:都对得上就回已记下的结论(refundReason 取这笔付款的
     * 退款待办,没有待办即 null),不写任何东西;对不上回 409,这是适配器的错,不替它猜。已记下的几行互相对不上回 503:
     * 例如异常付款丢了退款待办,照常重放就会回 refundReason=null,被适配器当成订单本身的付款。
     */
    private CommerceApi.PaymentOutcome replay(Stored order, CommerceApi.Payment in, KnownPayment known) {
        if (!known.consistent()) {
            throw new CommerceException(503, "付款回执与异常付款记录或退款待办不一致");
        }
        if (!in.orderId().equals(known.order())) {
            throw new CommerceException(409, "支付流水已用于其他订单");
        }
        if (known.amountMinor() != in.amountMinor() || !known.currency().equals(in.currency())) {
            throw new CommerceException(409, "支付流水已按不同金额或币种记录");
        }
        return new CommerceApi.PaymentOutcome(view(order), known.refundReason());
    }

    /**
     * 某个交易键已有的记录:boundOrder 是它绑定的订单(异常付款为 null);abnormalOf / abnormalReason 是它作为异常付款冲的那张订单
     * 和原因(绑定的付款没有这一行,都为 null);refundReason 是它的退款待办的原因(没有待办为 null)。
     */
    private record KnownPayment(String boundOrder, String abnormalOf, CommerceApi.RefundReason abnormalReason,
                                long amountMinor, String currency, CommerceApi.RefundReason refundReason) {
        /** 这笔付款记在哪张订单名下:绑定的那张,或异常付款冲的那张。 */
        String order() { return boundOrder != null ? boundOrder : abnormalOf; }

        /**
         * 同一事务写下的几行必须自洽。绑定订单的回执:没有异常付款行,退款待办没有或是订单本身的退款。
         * 不绑定订单的回执:恰有一行异常付款,退款待办与它同原因。
         */
        boolean consistent() {
            if (boundOrder != null) {
                return abnormalOf == null && (refundReason == null || refundReason.refundsOrdersOwnPayment());
            }
            return abnormalOf != null && abnormalReason != null && !abnormalReason.refundsOrdersOwnPayment()
                && abnormalReason == refundReason;
        }
    }
    private KnownPayment knownPayment(CommerceApi.Payment in) {
        var rows = jdbc.query("SELECT p.order_sn,a.order_sn,a.reason,p.amount_minor,p.currency,r.source FROM commerce_payment_receipt p "
                + "LEFT JOIN commerce_abnormal_payment a ON a.channel=p.channel AND a.transaction_id=p.transaction_id "
                + "LEFT JOIN commerce_refund_intent r ON r.channel=p.channel AND r.transaction_id=p.transaction_id "
                + "WHERE p.channel=? AND p.transaction_id=?",
            (row, index) -> new KnownPayment(row.getString(1), row.getString(2), storedReason(row.getString(3)),
                row.getLong(4), row.getString(5), storedReason(row.getString(6))),
            ascii(in.channel()), ascii(in.transactionId()));
        return rows.isEmpty() ? null : rows.getFirst();
    }
    private static CommerceApi.RefundReason storedReason(String value) {
        return value == null ? null : CommerceApi.RefundReason.stored(value);
    }

    private Stored byRequest(String buyer, String requestId) {
        List<Stored> rows = jdbc.query("SELECT * FROM oms_order WHERE buyer_id=? AND request_id=?", (rs, row) -> stored(rs), buyer, ascii(requestId));
        return rows.isEmpty() ? null : rows.get(0);
    }
    private Stored byId(String id, boolean lock) {
        List<Stored> rows = jdbc.query("SELECT * FROM oms_order WHERE order_sn=?" + (lock ? " FOR UPDATE" : ""), (rs, row) -> stored(rs), id);
        return rows.isEmpty() ? null : rows.get(0);
    }
    private Stored owned(Stored order, String buyer) {
        if (order == null || !order.buyer().equals(buyer)) { throw new CommerceException(404, "订单不存在"); }
        return order;
    }
    private CommerceApi.Order sameRequest(Stored prior, CommerceApi.CreateOrder in) {
        if (prior.skuId() != in.skuId() || prior.quantity() != in.quantity()) { throw new CommerceException(409, "请求号已用于不同下单参数"); }
        var original = officialOwner(prior.model().getOrderSn());
        if (!original.account().equals(in.buyerAccount()) || !original.revision().equals(in.buyerOwnerRevision())) {
            throw new CommerceException(409, "请求号的原账号或归属修订不符");
        }
        return view(prior);
    }
    private Stored officialOwned(CommerceApi.OwnedOrder input, String buyer, boolean lock) {
        if (input == null) { throw new CommerceException(400, "订单身份缺失"); }
        requireOrderId(input.orderId()); CommerceIdentity.account(input.buyerAccount()); CommerceIdentity.revision(input.buyerOwnerRevision());
        Stored order = owned(byId(input.orderId(), lock), buyer);
        if (order.skuId() == 0) { throw new CommerceException(404, "请通过原账号的寄售订单入口操作"); }
        var original = officialOwner(input.orderId());
        if (!original.account().equals(input.buyerAccount()) || !original.revision().equals(input.buyerOwnerRevision())) {
            throw new CommerceException(404, "订单不存在");
        }
        return order;
    }
    private record OfficialOwner(String account, String revision) {}
    private OfficialOwner officialOwner(String orderId) {
        var rows = jdbc.query("SELECT buyer_account,buyer_owner_revision FROM commerce_official_owner WHERE order_sn=?",
            (row,index)->new OfficialOwner(CommerceIdentity.storedAccount(row.getBytes(1)),CommerceIdentity.storedRevision(row.getBytes(2))),orderId);
        if (rows.size() != 1) { throw new CommerceException(503, "官方订单原身份缺失"); }
        return rows.getFirst();
    }
    private CommerceApi.Order view(Stored value) {
        return new CommerceApi.Order(value.model().getOrderSn(), value.buyer(), value.skuId(), value.quantity(), value.model().getPayAmount().longValueExact(), value.currency(), stateName(value.model().getStatus()), value.deadline());
    }

    /**
     * oms_order.status 的对外名称。8 / 9 是渠道修订付款之后的发放收敛(PaymentRevisionStore):REVERSING 收敛中
     * (撤销后扣回,或撤销被撤回后补发)、REVERSED 已扣回;收敛完成且该持有时回到 DELIVERED。
     */
    static String stateName(int status) {
        return switch (status) { case 0 -> "AWAITING_PAYMENT"; case 1 -> "DELIVERY_PENDING"; case 2 -> "DELIVERED"; case 4 -> "CANCELLED"; case 5 -> "REFUND_PENDING"; case 6 -> "REFUNDED"; case 7 -> "MANUAL"; case 8 -> "REVERSING"; case 9 -> "REVERSED"; default -> throw new CommerceException(503, "订单状态无法处理"); };
    }
    private static void requireOrderId(String id) {
        if (id == null || !id.matches("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")) { throw new CommerceException(400, "订单号非法"); }
    }
    private Sku sku(ResultSet rs) throws SQLException {
        PmsSkuStock value = new PmsSkuStock(); value.setId(rs.getLong("id")); value.setProductId(rs.getLong("product_id"));
        value.setSkuCode(rs.getString("sku_code")); value.setStock(rs.getInt("stock")); value.setLockStock(rs.getInt("lock_stock")); value.setPrice(BigDecimal.valueOf(rs.getLong("price_minor")));
        return new Sku(value, rs.getString("currency"), rs.getInt("published") == 1);
    }
    private Stored stored(ResultSet rs) throws SQLException {
        OmsOrder value = new OmsOrder(); value.setId(rs.getLong("id")); value.setOrderSn(rs.getString("order_sn"));
        value.setPayAmount(BigDecimal.valueOf(rs.getLong("pay_amount"))); value.setStatus(rs.getInt("status")); value.setCreateTime(new Date(rs.getLong("create_time_ms")));
        return new Stored(value, rs.getString("buyer_id"), rs.getLong("sku_id"), rs.getInt("quantity"), rs.getString("currency"), rs.getLong("payment_deadline_ms"));
    }
    private record Sku(PmsSkuStock model, String currency, boolean published) {}
    private record Stored(OmsOrder model, String buyer, long skuId, int quantity, String currency, long deadline) {}
    private static byte[] ascii(String value) { return value.getBytes(StandardCharsets.US_ASCII); }
}
