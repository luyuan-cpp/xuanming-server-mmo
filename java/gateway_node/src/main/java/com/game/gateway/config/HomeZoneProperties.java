package com.game.gateway.config;

import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.context.annotation.Configuration;

import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * 本网关副本负责的区:{@code gateway.home-zone-ids}(2026-11-11,服务器缺口清单第 29 条)。
 *
 * <p>网关按 zone 部署,但区服列表与 assign-gate 面向全部区。凡是「替某个区往共享存储里发布一份东西」的组件,
 * 只发布这里列出的区,每个区的写入方因此只有本区的网关副本,写量随 zone 数线性增长;读方仍可以读全部区。
 * 用到它的发布者:
 * <ul>
 *   <li>{@code service.ZoneHealthPublisher}:本区健康汇总写进 etcd(docs/design/gateway-zone-health-publish-20261111.md);</li>
 *   <li>{@code service.ZonePolicyPublisher}:本区 zone_config 的容量与建角开关写进 etcd 的固定键
 *       {@code /mmorpg/zone_policy/zone/<zoneId>}(不挂租约,内容变了才写),给本区 login 读。</li>
 * </ul>
 *
 * <p>取值:
 * <ul>
 *   <li>K8s:每个 zone namespace 的网关 ConfigMap 写 {@code [<本 zone>]}(tools/scripts/k8s_deploy.ps1 的 New-JavaSvcConfigMapYaml,
 *       契约测试钉住)。挂载的配置与 jar 内配置按属性合并,列表整体替换,不会与 jar 内缺省拼在一起。</li>
 *   <li>本机:一个网关进程服务全部本机区,jar 内 application.yaml 写 {@code [1, 2, 3]},与同文件 login.grpc.endpoints 的三个区一致。</li>
 *   <li>空列表:这个副本只读、不发布。</li>
 * </ul>
 * 区号必须为正且不重复,否则绑定失败、拒绝启动(fail-closed):写错一个区号,会让本该由本区发布的记录没人写。
 */
@Configuration
@ConfigurationProperties(prefix = "gateway")
public class HomeZoneProperties {

    private List<Long> homeZoneIds = List.of();

    /** 本副本负责的区号,按配置顺序,不可修改;没配时为空列表。 */
    public List<Long> getHomeZoneIds() {
        return homeZoneIds;
    }

    public void setHomeZoneIds(List<Long> homeZoneIds) {
        if (homeZoneIds == null) {
            this.homeZoneIds = List.of();
            return;
        }
        Set<Long> seen = new HashSet<>();
        for (Long zoneId : homeZoneIds) {
            if (zoneId == null || zoneId <= 0) {
                throw new IllegalArgumentException("gateway.home-zone-ids must contain positive zone ids, got " + homeZoneIds);
            }
            if (!seen.add(zoneId)) {
                throw new IllegalArgumentException("gateway.home-zone-ids contains duplicate zone id " + zoneId);
            }
        }
        this.homeZoneIds = List.copyOf(homeZoneIds);
    }
}
