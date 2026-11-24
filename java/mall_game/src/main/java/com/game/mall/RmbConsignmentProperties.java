package com.game.mall;

import jakarta.annotation.PostConstruct;
import java.util.Set;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

/** 时长单位毫秒、费率单位万分比；首次寄售冻结配置，改配置不改旧单。 */
@Component
@ConfigurationProperties("commerce.consignment")
public final class RmbConsignmentProperties {
    private long noticeMs = 172_800_000;
    /** 分层公示：当 smallBatchMaxPriceFen > 0 时，价格不高于此阈值的 ITEMS/CURRENCY 标准批次采用 smallBatchNoticeMs；PET/CHARACTER 始终走完整 noticeMs。 */
    private long smallBatchMaxPriceFen = 0;
    private long smallBatchNoticeMs = 0;
    private long saleMs = 604_800_000;
    private long coolingMs = 259_200_000;
    private int feeBps = 500;
    private Set<Integer> currencies = Set.of(0);
    public long getNoticeMs() { return noticeMs; }
    public void setNoticeMs(long value) { noticeMs = value; }
    public long getSmallBatchMaxPriceFen() { return smallBatchMaxPriceFen; }
    public void setSmallBatchMaxPriceFen(long value) { smallBatchMaxPriceFen = value; }
    public long getSmallBatchNoticeMs() { return smallBatchNoticeMs; }
    public void setSmallBatchNoticeMs(long value) { smallBatchNoticeMs = value; }
    public long getSaleMs() { return saleMs; }
    public void setSaleMs(long value) { saleMs = value; }
    public long getCoolingMs() { return coolingMs; }
    public void setCoolingMs(long value) { coolingMs = value; }
    public int getFeeBps() { return feeBps; }
    public void setFeeBps(int value) { feeBps = value; }
    public Set<Integer> getCurrencies() { return currencies; }
    public void setCurrencies(Set<Integer> value) { currencies = value == null ? null : Set.copyOf(value); }

    /** 计算指定资产类型与价格在首次登记时应冻结的公示时长（毫秒）。 */
    public long noticeMsFor(String assetKind, long priceFen) {
        if (smallBatchMaxPriceFen > 0
                && ("ITEMS".equals(assetKind) || "CURRENCY".equals(assetKind))
                && priceFen > 0 && priceFen <= smallBatchMaxPriceFen) {
            return Math.min(smallBatchNoticeMs, noticeMs);
        }
        return noticeMs;
    }

    @PostConstruct public void validate() {
        long maximum = 366L * 86_400_000;
        if (noticeMs < 0 || noticeMs > maximum || smallBatchNoticeMs < 0 || smallBatchNoticeMs > maximum
            || smallBatchMaxPriceFen < 0 || smallBatchMaxPriceFen > CommerceStore.MAX_AMOUNT_MINOR
            || (smallBatchMaxPriceFen > 0 && smallBatchNoticeMs > noticeMs)
            || saleMs < 1 || saleMs > maximum || coolingMs < 0 || coolingMs > maximum
            || feeBps < 0 || feeBps > 10_000 || currencies == null
            || currencies.stream().anyMatch(value -> value == null || value < 0 || value > 1)) {
            throw new IllegalStateException("人民币寄售规则非法；绑定货币不能交易");
        }
    }
}
