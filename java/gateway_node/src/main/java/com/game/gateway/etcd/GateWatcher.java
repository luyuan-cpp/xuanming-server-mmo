package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import io.etcd.jetcd.Client;
import jakarta.annotation.PreDestroy;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.util.List;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;
import java.util.function.LongSupplier;

/**
 * gate / scene / login 节点在 etcd 里登记的 NodeInfo。每个前缀背后是一份 list + watch 的本地镜像({@link NodeInfoMirror}),
 * 稳态不整段读 etcd,只收变更事件。
 *
 * <p>读哪些前缀(2026-11-11,服务器缺口清单第 29 条,docs/design/gateway-zone-health-publish-20261111.md):
 * <ul>
 *   <li>gate、scene 只按 zone 读:{@code <Svc>.rpc/zone/<z>/},只给本副本负责的 zone 发布健康汇总用
 *       ({@code service.ZoneHealthPublisher})。网关不再 watch 全服的 gate / scene:那样每台 gate 每 10 秒一次的心跳会送到
 *       每一个网关副本,事件量随 zone 数平方增长。别的 zone 的健康读汇总记录({@link ZoneHealthWatcher})。</li>
 *   <li>login 读全服:客户端只有一个网关入口,任一网关都要能把任一 zone 的 assign-gate 转给那个 zone 的 login。
 *       login 的登记只在进程启动、失租时写,没有周期重写,全服 watch 只在 login 启停时有事件。</li>
 * </ul>
 */
@Component
public class GateWatcher {

    private final Client etcdClient;
    private final long listTimeoutMs;
    private final LongSupplier monotonicMs;
    private final NodeInfoMirror logins;
    /** zone → 该 zone 的 gate / scene 镜像;第一次读这个 zone 时建。 */
    private final ConcurrentMap<Long, NodeInfoMirror> zoneGates = new ConcurrentHashMap<>();
    private final ConcurrentMap<Long, NodeInfoMirror> zoneScenes = new ConcurrentHashMap<>();
    private volatile boolean closed;

    /** 两个构造函数并存时 Spring 只认标了 {@code @Autowired} 的那个,不标就去找无参构造函数而拒启。 */
    @Autowired
    public GateWatcher(Client etcdClient, GateProperties gateProps) {
        this(etcdClient, gateProps, () -> System.nanoTime() / 1_000_000L);
    }

    /** 包内可见:单测注入单调时钟。 */
    GateWatcher(Client etcdClient, GateProperties gateProps, LongSupplier monotonicMs) {
        this.etcdClient = etcdClient;
        this.listTimeoutMs = gateProps.getDiscoveryTimeoutMs();
        this.monotonicMs = monotonicMs;
        this.logins = new NodeInfoMirror(etcdClient, NodeType.LOGIN_PREFIX, listTimeoutMs, monotonicMs);
    }

    /**
     * etcd 查询本身失败（超时/不可用）时抛出。
     * <p>
     * 必须区分「etcd 查询失败」与「该前缀确实没有节点」:后者才是空列表。
     * 旧实现把查询失败也吞成空列表,下游 ZoneHealthProbeService 会把所有
     * zone 判成 DOWN,选服界面全区变「维护中」—— 一次 etcd 抖动放大成
     * 全服可见的假故障。
     */
    public static class NodeDiscoveryException extends RuntimeException {
        public NodeDiscoveryException(String message, Throwable cause) {
            super(message, cause);
        }
    }

    /**
     * {@code zoneId} 当前登记的 gate 节点(只读 {@code GateNodeService.rpc/zone/<zoneId>/})。
     *
     * @throws NodeDiscoveryException 镜像需要重新整段读而 etcd 查询失败,或任一 NodeInfo 记录无法解析;
     *                                真正的零节点仍返回空列表
     */
    public List<NodeInfoRecord> fetchZoneGateNodes(long zoneId) {
        return zoneMirror(zoneGates, NodeType.GATE_PREFIX, zoneId).nodes();
    }

    /**
     * {@code zoneId} 当前登记的 scene 节点(只读 {@code SceneNodeService.rpc/zone/<zoneId>/})。
     *
     * @throws NodeDiscoveryException 同 {@link #fetchZoneGateNodes(long)}
     */
    public List<NodeInfoRecord> fetchZoneSceneNodes(long zoneId) {
        return zoneMirror(zoneScenes, NodeType.SCENE_PREFIX, zoneId).nodes();
    }

    /**
     * 全服当前登记的 go-zero login 节点。{@link com.game.gateway.grpc.LoginNodeDiscovery} 用它让按 zone 分的
     * login.rpc 连接池跟上实际部署,不靠手写的 {@code zoneId=host:port} 配置;区服健康的读方用它区分
     * 「这个 zone 只是网关不在」与「这个 zone 整个不在」。
     *
     * @throws NodeDiscoveryException 同 {@link #fetchZoneGateNodes(long)}
     */
    public List<NodeInfoRecord> fetchAllLoginNodes() {
        return logins.nodes();
    }

    /** 一类节点在一个 zone 下的发现前缀,与 C++ 节点的登记键 {@code <Svc>.rpc/zone/<z>/node_type/<t>/node_id/<n>} 对应。 */
    static String zonePrefix(String typePrefix, long zoneId) {
        return typePrefix + "zone/" + zoneId + "/";
    }

    private NodeInfoMirror zoneMirror(ConcurrentMap<Long, NodeInfoMirror> mirrors, String typePrefix, long zoneId) {
        if (zoneId <= 0) {
            throw new IllegalArgumentException("zone id must be positive, got " + zoneId);
        }
        if (closed) {
            throw new NodeDiscoveryException("node watcher is closed", null);
        }
        return mirrors.computeIfAbsent(zoneId,
                z -> new NodeInfoMirror(etcdClient, zonePrefix(typePrefix, z), listTimeoutMs, monotonicMs));
    }

    /** 关掉全部 watch;etcd Client 由它自己的 bean 关(Spring 先销毁依赖方,本 bean 在 Client 之前销毁)。 */
    @PreDestroy
    public void close() {
        closed = true;
        logins.close();
        zoneGates.values().forEach(NodeInfoMirror::close);
        zoneScenes.values().forEach(NodeInfoMirror::close);
    }
}
