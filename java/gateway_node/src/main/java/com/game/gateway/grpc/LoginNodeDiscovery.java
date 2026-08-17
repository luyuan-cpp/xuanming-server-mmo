package com.game.gateway.grpc;

import com.game.gateway.config.LoginGrpcProperties;
import com.game.gateway.etcd.GateWatcher;
import com.game.gateway.etcd.NodeInfoRecord;
import com.game.gateway.etcd.NodeType;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * 周期性地把 etcd 里的 login 节点注册({@code LoginNodeService.rpc/zone/...})
 * 同步进 {@link LoginRpcClient} 的动态路由表,取代手工维护的
 * {@code login.grpc.endpoints: "<zoneId>=host:port"} 静态映射。
 *
 * <p>多节点语义:
 * <ul>
 *   <li>同一 zone 注册多个 login 实例 → 全部进池,按轮询分摊(重试自动
 *       failover 到另一实例)。</li>
 *   <li>新增 zone / login 扩缩容 → 下一个轮询周期自动生效,网关无需改配置
 *       或重启。</li>
 *   <li>etcd 查询失败 → 保留 last-known-good 路由(不清表),与
 *       ZoneHealthProbeService 的降级策略一致;静态配置始终作为兜底。</li>
 * </ul>
 */
@Component
public class LoginNodeDiscovery {

    private static final Logger log = LoggerFactory.getLogger(LoginNodeDiscovery.class);

    private final GateWatcher watcher;
    private final LoginRpcClient rpc;
    private final LoginGrpcProperties props;

    /** 上次成功同步的 zone->实例数,仅用于变化时打日志。 */
    private Map<Integer, Integer> lastSnapshot = Map.of();

    public LoginNodeDiscovery(GateWatcher watcher, LoginRpcClient rpc, LoginGrpcProperties props) {
        this.watcher = watcher;
        this.rpc = rpc;
        this.props = props;
    }

    @Scheduled(fixedDelayString = "${login.grpc.discovery-interval-ms:5000}")
    public void refresh() {
        if (!props.isDiscoveryEnabled()) {
            return;
        }

        final List<NodeInfoRecord> nodes;
        try {
            nodes = watcher.fetchAllLoginNodes();
        } catch (GateWatcher.NodeDiscoveryException e) {
            // 保留 last-known-good:不调用 updateDynamicEndpoints,已建 channel
            // 继续服务;真正的节点下线由下一次成功查询收敛。
            log.warn("login.rpc discovery skipped (etcd unavailable): {}", e.getMessage());
            return;
        }

        Map<Integer, List<String>> endpointsByZone = new LinkedHashMap<>();
        for (NodeInfoRecord node : nodes) {
            if (node.getNodeType() != NodeType.LOGIN_NODE_SERVICE) continue;
            NodeInfoRecord.Endpoint ep = node.getEndpoint();
            if (ep == null || ep.getIp() == null || ep.getIp().isBlank() || ep.getPort() <= 0) {
                log.warn("login.rpc discovery: node {} in zone {} has no usable endpoint, skipped",
                        node.getNodeId(), node.getZoneId());
                continue;
            }
            endpointsByZone
                    .computeIfAbsent((int) node.getZoneId(), z -> new ArrayList<>())
                    .add(ep.getIp() + ":" + ep.getPort());
        }

        rpc.updateDynamicEndpoints(endpointsByZone);

        Map<Integer, Integer> snapshot = rpc.dynamicRoutingSnapshot();
        // 集成测试里 LoginRpcClient 是 Mockito mock,快照可能为 null。
        if (snapshot != null && !snapshot.equals(lastSnapshot)) {
            log.info("login.rpc discovery routing changed: {} (was {})", snapshot, lastSnapshot);
            lastSnapshot = snapshot;
        }
    }
}
