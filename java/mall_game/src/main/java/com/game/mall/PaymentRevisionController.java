package com.game.mall;

import jakarta.servlet.http.HttpServletRequest;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

/** 渠道修订付款的入口(payment 角色,契约见 CommerceApi.Revision 与 PaymentRevisionStore)。 */
@RestController
public final class PaymentRevisionController {
    private final PaymentRevisionStore store;

    public PaymentRevisionController(PaymentRevisionStore store) {
        this.store = store;
    }

    @PostMapping("/internal/payments/revision")
    public CommerceApi.RevisionOutcome revision(HttpServletRequest request, @RequestBody CommerceApi.Revision input) {
        // 经济权限在提交入口再次检查,与 /internal/payments 相同:不能让容器路由前缀或路径适配改变信任边界。
        if (!"payment".equals(request.getAttribute(HmacAuthentication.ROLE))) {
            throw new CommerceException(401, "支付调用方认证失败");
        }
        return store.record(input);
    }
}
