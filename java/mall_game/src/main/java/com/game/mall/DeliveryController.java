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
    @PostMapping("/internal/refunds/poll")
    public List<CommerceApi.Payment> refunds(HttpServletRequest request) {
        payment(request);
        return store.refunds();
    }
    // 回执必须带 paymentTransactionId(被退的是哪一笔):一张订单可能同时有本单付款和重复付款两笔待退。
    @PostMapping("/internal/refunds/receipt")
    public void refunded(HttpServletRequest request, @RequestBody CommerceApi.Refund input) {
        payment(request);
        store.refunded(input);
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
