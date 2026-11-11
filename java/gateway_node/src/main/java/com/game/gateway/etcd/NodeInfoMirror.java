package com.game.gateway.etcd;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.etcd.GateWatcher.NodeDiscoveryException;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.function.LongSupplier;

/**
 * 一个 etcd 前缀下 NodeInfo 记录的本地镜像(list + watch,引擎见 {@link EtcdPrefixMirror})。本类只管 NodeInfo 的解码:
 * <ul>
 *   <li>{@code <prefix>allocated/} 下的发号哨兵不是 NodeInfo,跳过;</li>
 *   <li>任何一条真实 NodeInfo 解析失败 → 读失败,直到那条记录被改好或删掉;</li>
 *   <li>返回顺序按键排序。</li>
 * </ul>
 *
 * <p>2026-11-05(服务器缺口清单第 29 条)由每次整段读改成镜像;2026-11-11 list + watch 引擎抽到 {@link EtcdPrefixMirror},
 * 读语义不变。前缀可以是全服的 {@code <Svc>.rpc/},也可以是一个 zone 的 {@code <Svc>.rpc/zone/<z>/}(见 {@link GateWatcher})。
 */
final class NodeInfoMirror implements AutoCloseable {

    /** 镜像最长用多久就强制重新整段读一次(毫秒),同 {@link EtcdPrefixMirror#MAX_AGE_MS}。 */
    static final long MAX_AGE_MS = EtcdPrefixMirror.MAX_AGE_MS;

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final EtcdPrefixMirror<NodeInfoRecord> mirror;

    /**
     * @param listTimeoutMs 一次整段读最长等多久
     * @param monotonicMs   单调时钟(毫秒),只用来算镜像年龄
     */
    NodeInfoMirror(Client etcd, String prefix, long listTimeoutMs, LongSupplier monotonicMs) {
        // {Gate,Scene}NodeService.rpc/ 前缀下有两类键:
        //   - "<svc>/zone/<z>/node_type/<t>/node_id/<n>" -> NodeInfo JSON
        //   - "<svc>/allocated/node_type/<t>/node_id/<n>" -> 发号哨兵(不是 JSON)
        // 只要前一类。按 zone 收窄的前缀下没有发号哨兵,这条规则对它恒不命中。
        String sentinelPrefix = prefix + "allocated/";
        this.mirror = new EtcdPrefixMirror<>(etcd, prefix, "NodeInfo",
                (key, value) -> decode(sentinelPrefix, key, value), listTimeoutMs, monotonicMs);
    }

    /**
     * 当前全部 NodeInfo 记录(按键排序,不可修改)。
     *
     * @throws NodeDiscoveryException 需要整段读而 etcd 查询失败,或有记录无法解析;真正的零节点返回空列表
     */
    List<NodeInfoRecord> nodes() {
        List<EtcdPrefixMirror.Entry<NodeInfoRecord>> entries = mirror.entries();
        List<NodeInfoRecord> nodes = new ArrayList<>(entries.size());
        for (EtcdPrefixMirror.Entry<NodeInfoRecord> entry : entries) {
            nodes.add(entry.value());
        }
        return List.copyOf(nodes);
    }

    @Override
    public void close() {
        mirror.close();
    }

    private static NodeInfoRecord decode(String sentinelPrefix, String key, ByteSequence value) throws IOException {
        if (key.startsWith(sentinelPrefix)) {
            return null;
        }
        NodeInfoRecord info = MAPPER.readValue(value.toString(StandardCharsets.UTF_8), NodeInfoRecord.class);
        if (info == null) {
            throw new IllegalArgumentException("NodeInfo JSON resolved to null");
        }
        return info;
    }
}
