package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import com.game.gateway.etcd.GateWatcher.NodeDiscoveryException;
import io.etcd.jetcd.Client;
import jakarta.annotation.PreDestroy;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.util.HashMap;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * 读全部 zone 的健康汇总({@link ZoneHealthRecord},2026-11-11,服务器缺口清单第 29 条):对 {@link ZoneHealthRecord#PREFIX}
 * 做 list + watch({@link EtcdPrefixMirror}),稳态只收汇总的变更。汇总只在显示会变时才写,读方收到的事件与 gate 心跳无关
 * (docs/design/gateway-zone-health-publish-20261111.md §6)。
 *
 * <p>一个 zone 通常有两条记录(本 zone 两个网关副本各写一条),取 ModRevision 最大的那条:最近一次写入反映最近一次观察到的变化;
 * watch 卡住的发布者不再写,自然让位给另一个。
 */
@Component
public class ZoneHealthWatcher {

    private final EtcdPrefixMirror<ZoneHealthRecord> mirror;

    @Autowired
    public ZoneHealthWatcher(Client etcdClient, GateProperties gateProps) {
        this(etcdClient, gateProps, () -> System.nanoTime() / 1_000_000L);
    }

    /** 包内可见:单测注入单调时钟。 */
    ZoneHealthWatcher(Client etcdClient, GateProperties gateProps, LongSupplier monotonicMs) {
        this.mirror = new EtcdPrefixMirror<>(etcdClient, ZoneHealthRecord.PREFIX, "zone health",
                ZoneHealthRecord::decode, gateProps.getDiscoveryTimeoutMs(), monotonicMs);
    }

    /**
     * 每个 zone 最近写入的那条汇总(不可修改);没有任何记录的 zone 不在结果里。
     *
     * @throws NodeDiscoveryException 需要整段读而 etcd 查询失败,或有记录无法解码(整次读失败,不返回部分结果)
     */
    public Map<Long, ZoneHealthRecord> latestByZone() {
        Map<Long, EtcdPrefixMirror.Entry<ZoneHealthRecord>> latest = new HashMap<>();
        for (EtcdPrefixMirror.Entry<ZoneHealthRecord> entry : mirror.entries()) {
            // 记录按键排序;ModRevision 相同(只在单测里会出现)时保留先到的那条,结果确定。
            latest.merge(entry.value().zoneId(), entry,
                    (kept, candidate) -> candidate.modRevision() > kept.modRevision() ? candidate : kept);
        }
        Map<Long, ZoneHealthRecord> byZone = new HashMap<>();
        latest.forEach((zoneId, entry) -> byZone.put(zoneId, entry.value()));
        return Map.copyOf(byZone);
    }

    /** 关掉 watch;etcd Client 由它自己的 bean 关。 */
    @PreDestroy
    public void close() {
        mirror.close();
    }
}
