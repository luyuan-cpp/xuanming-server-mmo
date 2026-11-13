package com.game.mall;

import jakarta.annotation.PostConstruct;
import java.nio.charset.StandardCharsets;
import java.util.Set;
import org.springframework.boot.context.properties.ConfigurationProperties;

@ConfigurationProperties("commerce")
public final class CommerceProperties {
    private boolean requireTidb = true;
    private String runMode = "prod";
    private String gatewaySecret;
    private String paymentSecret;
    private String deliverySecret;
    private String adminSecret;
    private String adminSubject = "mall-admin";
    private boolean mockPaymentEnabled;
    private Set<String> allowedPaymentChannels = Set.of();
    public boolean isMockPaymentEnabled() { return mockPaymentEnabled; }
    public void setMockPaymentEnabled(boolean value) { mockPaymentEnabled = value; }
    public Set<String> getAllowedPaymentChannels() { return allowedPaymentChannels; }
    public void setAllowedPaymentChannels(Set<String> value) { allowedPaymentChannels = value == null ? Set.of() : Set.copyOf(value); }
    void requireMockPayment() {
        if (!mockPaymentEnabled || !("dev".equals(runMode) || "test".equals(runMode))) {
            throw new CommerceException(503, "本地mock支付未启用或运行模式不允许");
        }
    }
    void requirePaymentChannel(String channel) {
        if ("mock".equals(channel)) { requireMockPayment(); return; }
        if (!allowedPaymentChannels.contains(channel)) { throw new CommerceException(403, "支付渠道未配置允许"); }
    }
    public String getAdminSecret(){return adminSecret;}
    public void setAdminSecret(String value){adminSecret=value;}
    public String getAdminSubject(){return adminSubject;}
    public void setAdminSubject(String value){adminSubject=value;}
    public String getDeliverySecret(){return deliverySecret;}
    public void setDeliverySecret(String value){deliverySecret=value;}
    private String paymentSubject = "payment-adapter";
    private int maxUnpaidOrders = 5;
    public boolean isRequireTidb() { return requireTidb; }
    public void setRequireTidb(boolean value) { requireTidb = value; }
    public String getRunMode() { return runMode; }
    public void setRunMode(String value) { runMode = value; }
    public String getGatewaySecret() { return gatewaySecret; }
    public void setGatewaySecret(String value) { gatewaySecret = value; }
    public String getPaymentSecret() { return paymentSecret; }
    public void setPaymentSecret(String value) { paymentSecret = value; }
    public String getPaymentSubject() { return paymentSubject; }
    public void setPaymentSubject(String value) { paymentSubject = value; }
    public int getMaxUnpaidOrders() { return maxUnpaidOrders; }
    public void setMaxUnpaidOrders(int value) { maxUnpaidOrders = value; }
    @PostConstruct public void validate() {
        if (!java.util.Set.of("prod", "dev", "test").contains(runMode) || (runMode.equals("prod") && !requireTidb)) {
            throw new IllegalStateException("商城运行模式非法，prod 不允许关闭 TiDB 权威存储要求");
        }
        if (!strong(gatewaySecret) || !strong(paymentSecret) || gatewaySecret.equals(paymentSecret)) {
            throw new IllegalStateException("必须配置两把不同且至少 32 字节的商城调用密钥");
        }
        if(deliverySecret!=null&&!deliverySecret.isEmpty()&&(!strong(deliverySecret)||deliverySecret.equals(gatewaySecret)||deliverySecret.equals(paymentSecret)))throw new IllegalStateException("交付调用密钥必须与网关及支付密钥不同且至少32字节");
        if(adminSecret!=null&&!adminSecret.isEmpty()&&(!strong(adminSecret)||adminSecret.equals(gatewaySecret)||adminSecret.equals(paymentSecret)||adminSecret.equals(deliverySecret)))throw new IllegalStateException("管理调用密钥必须独立且至少32字节");
        if(adminSubject==null||!adminSubject.matches("[a-z][a-z0-9-]{2,63}"))throw new IllegalStateException("管理调用方名称非法");
        if (paymentSubject == null || !paymentSubject.matches("[a-z][a-z0-9-]{2,63}")) {
            throw new IllegalStateException("支付调用方名称非法");
        }
        if (maxUnpaidOrders < 1 || maxUnpaidOrders > 100) { throw new IllegalStateException("未支付订单上限必须在 1..100"); }
        if (mockPaymentEnabled && "prod".equals(runMode)) { throw new IllegalStateException("prod禁止启用本地mock支付"); }
        if (allowedPaymentChannels.size() > 32 || allowedPaymentChannels.stream().anyMatch(value ->
                !value.matches("[a-z][a-z0-9_-]{1,31}") || value.equals("mock"))) {
            throw new IllegalStateException("支付渠道允许集合非法；mock只能走独立事实适配器");
        }
        // devpay 是支付服务的本地联调渠道(go/payment,只在 dev / test 开),付款事实由同一份密钥自签,不能当真钱入账。
        if ("prod".equals(runMode) && allowedPaymentChannels.stream().anyMatch(value -> value.equals("devpay") || value.startsWith("mock"))) {
            throw new IllegalStateException("prod禁止允许本地联调支付渠道");
        }
    }
    private boolean strong(String value) { return value != null && value.getBytes(StandardCharsets.UTF_8).length >= 32; }
}
