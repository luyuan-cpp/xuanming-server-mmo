package com.game.gateway.etcd;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.config.GateProperties;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Component;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

@Component
public class GateWatcher {

    private static final Logger log = LoggerFactory.getLogger(GateWatcher.class);
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final Client etcdClient;
    private final GateProperties gateProps;

    public GateWatcher(Client etcdClient, GateProperties gateProps) {
        this.etcdClient = etcdClient;
        this.gateProps = gateProps;
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
     * Fetches all gate nodes currently registered in etcd.
     *
     * @throws NodeDiscoveryException etcd 查询失败或任一 NodeInfo 记录无法解析；
     *                                真正的零节点仍返回空列表
     */
    public List<NodeInfoRecord> fetchAllGateNodes() {
        return fetchNodesByPrefix(NodeType.GATE_PREFIX);
    }

    /**
     * Fetches all scene nodes currently registered in etcd.
     *
     * @throws NodeDiscoveryException etcd 查询失败或任一 NodeInfo 记录无法解析；
     *                                真正的零节点仍返回空列表
     */
    public List<NodeInfoRecord> fetchAllSceneNodes() {
        return fetchNodesByPrefix(NodeType.SCENE_PREFIX);
    }

    /**
     * Fetches all go-zero login nodes currently registered in etcd. Used by
     * {@link com.game.gateway.grpc.LoginNodeDiscovery} to keep the per-zone
     * login.rpc channel pool in sync with the actual deployment instead of a
     * hand-maintained {@code zoneId=host:port} config list.
     *
     * @throws NodeDiscoveryException etcd 查询失败或任一 NodeInfo 记录无法解析；
     *                                真正的零节点仍返回空列表
     */
    public List<NodeInfoRecord> fetchAllLoginNodes() {
        return fetchNodesByPrefix(NodeType.LOGIN_PREFIX);
    }

    private List<NodeInfoRecord> fetchNodesByPrefix(String prefix) {
        final GetResponse resp;
        try {
            ByteSequence prefixKey = ByteSequence.from(prefix, StandardCharsets.UTF_8);
            GetOption option = GetOption.builder().isPrefix(true).build();
            resp = etcdClient.getKVClient()
                    .get(prefixKey, option)
                    .get(gateProps.getDiscoveryTimeoutMs(), TimeUnit.MILLISECONDS);
        } catch (Exception e) {
            log.error("Failed to fetch nodes with prefix {}: {}", prefix, e.getMessage());
            throw new NodeDiscoveryException("etcd query failed for prefix " + prefix, e);
        }

        List<NodeInfoRecord> nodes = new ArrayList<>();
        for (KeyValue kv : resp.getKvs()) {
            String key = kv.getKey().toString(StandardCharsets.UTF_8);
            // The {Gate,Scene}NodeService.rpc/ prefix houses two key families:
            //   - "<svc>/zone/<z>/node_type/<t>/node_id/<n>"  -> NodeInfo JSON
            //   - "<svc>/allocated/node_type/<t>/node_id/<n>" -> snowflake
            //     allocator sentinel (opaque bytes, NOT JSON).
            // We only care about the first. Ignore the allocator keys
            // instead of flooding the log with "Unexpected character" warns.
            if (key.startsWith(prefix + "allocated/")) {
                continue;
            }
            try {
                String json = kv.getValue().toString(StandardCharsets.UTF_8);
                NodeInfoRecord info = MAPPER.readValue(json, NodeInfoRecord.class);
                if (info == null) {
                    throw new IllegalArgumentException("NodeInfo JSON resolved to null");
                }
                nodes.add(info);
            } catch (Exception e) {
                // 返回部分列表会让下游把本次不完整探测发布成新的成功快照，
                // 覆盖 last-known-good。任何真实 NodeInfo 坏记录都必须让
                // 整批失败；allocator sentinel 已在上方明确排除。
                log.error("Failed to parse NodeInfo from key={}: {}", key, e.getMessage());
                throw new NodeDiscoveryException("invalid NodeInfo record at key " + key, e);
            }
        }
        return nodes;
    }
}
