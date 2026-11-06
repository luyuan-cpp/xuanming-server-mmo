package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import io.etcd.jetcd.Client;
import jakarta.annotation.PreDestroy;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.util.List;
import java.util.function.LongSupplier;

/**
 * gate / scene / login 节点在 etcd 里登记的 NodeInfo。
 *
 * <p>2026-11-05 起(服务器缺口清单第 29 条)每个前缀背后是一份 list + watch 的本地镜像({@link NodeInfoMirror}):
 * 健康探测与 login 发现每 5 秒读一次,稳态不再整段读 etcd,只收变更事件。读的语义不变,见各方法与 {@link NodeInfoMirror}。
 */
@Component
public class GateWatcher {

    private final NodeInfoMirror gates;
    private final NodeInfoMirror scenes;
    private final NodeInfoMirror logins;

    /** 两个构造函数并存时 Spring 只认标了 {@code @Autowired} 的那个,不标就去找无参构造函数而拒启。 */
    @Autowired
    public GateWatcher(Client etcdClient, GateProperties gateProps) {
        this(etcdClient, gateProps, () -> System.nanoTime() / 1_000_000L);
    }

    /** 包内可见:单测注入单调时钟。 */
    GateWatcher(Client etcdClient, GateProperties gateProps, LongSupplier monotonicMs) {
        long listTimeoutMs = gateProps.getDiscoveryTimeoutMs();
        this.gates = new NodeInfoMirror(etcdClient, NodeType.GATE_PREFIX, listTimeoutMs, monotonicMs);
        this.scenes = new NodeInfoMirror(etcdClient, NodeType.SCENE_PREFIX, listTimeoutMs, monotonicMs);
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
     * 全服当前登记的 gate 节点。
     *
     * @throws NodeDiscoveryException 镜像需要重新整段读而 etcd 查询失败,或任一 NodeInfo 记录无法解析;
     *                                真正的零节点仍返回空列表
     */
    public List<NodeInfoRecord> fetchAllGateNodes() {
        return gates.nodes();
    }

    /**
     * 全服当前登记的 scene 节点。
     *
     * @throws NodeDiscoveryException 同 {@link #fetchAllGateNodes()}
     */
    public List<NodeInfoRecord> fetchAllSceneNodes() {
        return scenes.nodes();
    }

    /**
     * 全服当前登记的 go-zero login 节点。{@link com.game.gateway.grpc.LoginNodeDiscovery} 用它让按 zone 分的
     * login.rpc 连接池跟上实际部署,不靠手写的 {@code zoneId=host:port} 配置。
     *
     * @throws NodeDiscoveryException 同 {@link #fetchAllGateNodes()}
     */
    public List<NodeInfoRecord> fetchAllLoginNodes() {
        return logins.nodes();
    }

    /** 关掉三个 watch;etcd Client 由它自己的 bean 关(Spring 先销毁依赖方,本 bean 在 Client 之前销毁)。 */
    @PreDestroy
    public void close() {
        gates.close();
        scenes.close();
        logins.close();
    }
}
