package com.game.mall;
import jakarta.servlet.http.HttpServletRequest;
import java.util.List;
import org.springframework.web.bind.annotation.*;
@RestController
public final class DeliveryController {
    private final DeliveryStore store;
    public DeliveryController(DeliveryStore store) {
        this.store = store;
    }
    @GetMapping("/api/skus")
    public DeliveryApi.Skus skus() {
        return store.skus();
    }
    @PostMapping("/api/skus/list")
    public DeliveryApi.Skus skus(@RequestBody DeliveryApi.SkuPage page) {
        return store.skus(page);
    }

    @PostMapping("/internal/deliveries/poll")
    public DeliveryApi.PollPage poll(HttpServletRequest request, @RequestBody DeliveryApi.Poll input) {
        check(request);
        return store.poll(input);
    }
    @PostMapping("/internal/deliveries/receipt")
    public void receipt(HttpServletRequest request, @RequestBody DeliveryApi.Receipt input) {
        check(request);
        store.receipt(input);
    }
    // 每项是要退的一笔付款与原因;适配器以这笔付款的交易号作渠道退款的幂等键(契约见 CommerceApi.PendingRefund)。
    @PostMapping("/internal/refunds/poll")
    public List<CommerceApi.PendingRefund> refunds(HttpServletRequest request) {
        payment(request);
        return store.refunds();
    }
    // 回执必须带 paymentTransactionId(被退的是哪一笔):一张订单可能同时有本单付款和几笔异常付款待退。
    @PostMapping("/internal/refunds/receipt")
    public void refunded(HttpServletRequest request, @RequestBody CommerceApi.Refund input) {
        payment(request);
        store.refunded(input);
    }
    // 渠道退不了的退款待办转人工(契约见 CommerceApi.RefundClosure)。
    @PostMapping("/internal/refunds/close")
    public void closeRefund(HttpServletRequest request, @RequestBody CommerceApi.RefundClosure input) {
        payment(request);
        store.closeRefund(input);
    }
    // 扣回待办与回执(付款被渠道修订之后的发放收敛,见 DeliveryApi.ReversalEntry)。
    @PostMapping("/internal/reversals/poll")
    public DeliveryApi.ReversalPage reversals(HttpServletRequest request, @RequestBody DeliveryApi.Poll input) {
        check(request);
        return store.reversals(input);
    }
    @PostMapping("/internal/reversals/receipt")
    public void reversalReceipt(HttpServletRequest request, @RequestBody DeliveryApi.ReversalReceipt input) {
        check(request);
        store.reversalReceipt(input);
    }
    private void payment(HttpServletRequest request) {
        if (!"payment".equals(request.getAttribute(HmacAuthentication.ROLE)))
            throw new CommerceException(401, "退款渠道调用方认证失败");
    }
    private void check(HttpServletRequest request) {
        if (!"delivery".equals(request.getAttribute(HmacAuthentication.ROLE)))
            throw new CommerceException(401, "交付调用方认证失败");
    }
}
