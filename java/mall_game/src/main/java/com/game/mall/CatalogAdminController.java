package com.game.mall;

import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.web.bind.annotation.*;

/**
 * 商城管理入口。调用方只有后台(java/gm_admin):HMAC 主体是管理调用方,写命令另带双人复核批准记录
 * (docs/design/admin-identity-and-dual-control.md §5.5)。批准记录缺省必须合格;只有本机开发显式放开才接受不带记录的写命令,
 * 每次都打 ERROR 并计入 mall_admin_unapproved_command_total。带了记录就必须合格,放开时也一样。
 */
@RestController
public final class CatalogAdminController {
    private static final Logger LOG = LoggerFactory.getLogger(CatalogAdminController.class);
    private final CatalogAdminStore store;
    private final CommerceStore orders;
    private final ManualAdminStore manual;
    private final boolean allowUnapproved;
    private final Counter unapproved;
    public CatalogAdminController(CatalogAdminStore store, CommerceStore orders, ManualAdminStore manual,
            CommerceProperties props, MeterRegistry meters) {
        this.store = store;
        this.orders = orders;
        this.manual = manual;
        this.allowUnapproved = props.isAdminAllowUnapproved();
        this.unapproved = Counter.builder("mall.admin.unapproved.command")
            .description("Mall admin write commands accepted without a dual-control approval record (only while commerce.admin-allow-unapproved is on).")
            .register(meters);
        if (allowUnapproved) {
            LOG.error("commerce.admin-allow-unapproved is on: mall admin write commands without a dual-control approval record are accepted; keep it off outside local development");
        }
    }

    @PostMapping("/internal/admin/order-view")
    public CommerceApi.Order order(HttpServletRequest request, @RequestBody CommerceApi.OrderQuery input) {
        actor(request);
        if (input == null) { throw new CommerceException(400, "订单查询参数缺失"); }
        return orders.paymentOrderView(input.orderId());
    }

    @PostMapping("/internal/admin/manual/list")
    public ManualAdminApi.Summary listManual(HttpServletRequest request, @RequestBody ManualAdminApi.ListQuery input) {
        actor(request);
        return manual.list(input);
    }

    @PostMapping("/internal/admin/manual/orders/resolve")
    public ManualAdminApi.Resolution resolveOrder(HttpServletRequest request, @RequestBody ManualAdminApi.ResolveOrder input) {
        return manual.resolveOrder(approved(request, input), input);
    }

    @PostMapping("/internal/admin/manual/refunds/resolve")
    public ManualAdminApi.Resolution resolveRefund(HttpServletRequest request, @RequestBody ManualAdminApi.ResolveRefund input) {
        return manual.resolveRefund(approved(request, input), input);
    }

    @PostMapping("/internal/admin/manual/consignments/resolve")
    public ManualAdminApi.Resolution resolveConsignment(HttpServletRequest request, @RequestBody ManualAdminApi.ResolveConsignment input) {
        return manual.resolveConsignment(approved(request, input), input);
    }

    @PostMapping("/internal/admin/manual/incomes/dispute")
    public ManualAdminApi.Resolution resolveIncomeDispute(HttpServletRequest request, @RequestBody ManualAdminApi.ResolveIncomeDispute input) {
        return manual.resolveIncomeDispute(approved(request, input), input);
    }

    @PostMapping("/internal/admin/skus/list")
    public CatalogAdminApi.Skus list(HttpServletRequest request, @RequestBody DeliveryApi.SkuPage input) {
        actor(request);
        return store.list(input);
    }
    @PostMapping("/internal/admin/skus/create")
    public CatalogAdminApi.Sku create(HttpServletRequest request, @RequestBody CatalogAdminApi.Create input) {
        return store.create(approved(request, input), input);
    }
    @PostMapping("/internal/admin/skus/update")
    public CatalogAdminApi.Sku update(HttpServletRequest request, @RequestBody CatalogAdminApi.Update input) {
        return store.update(approved(request, input), input);
    }
    @PostMapping("/internal/admin/skus/stock")
    public CatalogAdminApi.Sku stock(HttpServletRequest request, @RequestBody CatalogAdminApi.AdjustStock input) {
        return store.adjustStock(approved(request, input), input);
    }
    /** 写命令的准入:先认调用方,再核批准记录。拒绝发生在任何读写之前;返回 HMAC 主体(幂等表的 actor)。 */
    private String approved(HttpServletRequest request, AdminApproval.Command input) {
        String actor = actor(request);
        if (input == null) { throw new CommerceException(400, "管理命令参数缺失"); }
        if (input.approval() == null && allowUnapproved) {
            unapproved.increment();
            LOG.error("mall admin write command accepted without a dual-control approval record: route={} requestId={} (commerce.admin-allow-unapproved is on)",
                request.getRequestURI(), input.requestId());
            return actor;
        }
        String refusal = AdminApproval.refusal(input);
        if (refusal != null) { throw new CommerceException(403, refusal); }
        return actor;
    }
    private static String actor(HttpServletRequest request) {
        if (!"admin".equals(request.getAttribute(HmacAuthentication.ROLE))) {
            throw new CommerceException(401, "管理调用方认证失败");
        }
        return (String) request.getAttribute(HmacAuthentication.SUBJECT);
    }
}
