package com.game.gateway.service;

import com.game.gateway.dto.ServerListResponse;
import com.game.gateway.dto.ZoneDisplayStatus;
import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.springframework.stereotype.Component;

/**
 * 新玩家还有没有区可进(2026-11-12,区服满员处理 §6,docs/design/zone-full-handling.md)。导出两个 gauge:
 * <ul>
 *   <li>{@code gateway.zones.open}:区服列表里显示开放的区数;</li>
 *   <li>{@code gateway.zones.accepting_new_players}:其中没有暂停创建新角色的区数。</li>
 * </ul>
 * {@link RoleCapacityGuard} 会在无人值守时关上创角开关。最后一个能建角的区被关上之后,新玩家选哪个区都建不了角,
 * 该开新区了;告警 {@code GatewayNoZoneAcceptsNewCharacters}(deploy/k8s/gateway-zone-policy-alerts.yaml)靠这两个数发现:
 * 有开放的区、却没有一个能建角。全服维护(没有开放的区)不算。
 *
 * <p>口径与玩家看到的区服列表同源:取 {@link ServerListService#getServerList()} 的显示状态与
 * {@link ServerListService#acceptsNewPlayers},每次抓取现算(区服目录的行有 1 秒缓存,不额外查库)。
 * 每个网关副本看到的都是全部区,数相同,告警取 max。列表读不到(查库失败)时取值函数抛异常,Micrometer 记成 NaN,
 * 告警不响:读不到不等于一个都没有。
 */
@Component
public class NewPlayerCapacityMetrics {

    public NewPlayerCapacityMetrics(ServerListService serverList, MeterRegistry registry) {
        Gauge.builder("gateway.zones.open", serverList, NewPlayerCapacityMetrics::openZones)
                .description("Zones shown as open in the server list")
                .register(registry);
        Gauge.builder("gateway.zones.accepting_new_players", serverList, NewPlayerCapacityMetrics::zonesAcceptingNewPlayers)
                .description("Open zones in the server list where new characters can still be created")
                .register(registry);
    }

    static double openZones(ServerListService serverList) {
        ServerListResponse list = serverList.getServerList();
        return list.getZones().stream().filter(zone -> zone.getStatus() == ZoneDisplayStatus.OPEN).count();
    }

    static double zonesAcceptingNewPlayers(ServerListService serverList) {
        ServerListResponse list = serverList.getServerList();
        return list.getZones().stream().filter(ServerListService::acceptsNewPlayers).count();
    }
}
