package com.game.gateway.etcd;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.gateway.service.KillSwitchRuleStore;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.kv.DeleteResponse;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

/**
 * {@link KillSwitchRuleStore} 的 etcd 实现。每次请求有 {@code killswitch.etcd-timeout-ms} 的截止(缺省 3 秒),
 * 读写挂住时管理接口回 503,不让请求线程堆积。只给运维管理接口用,不在任何请求热路径上。
 */
@Component
public class EtcdKillSwitchRuleStore implements KillSwitchRuleStore {

    private static final ObjectMapper JSON = new ObjectMapper();

    private final Client etcd;
    private final long timeoutMs;

    public EtcdKillSwitchRuleStore(Client etcd, @Value("${killswitch.etcd-timeout-ms:3000}") long timeoutMs) {
        this.etcd = etcd;
        this.timeoutMs = timeoutMs;
    }

    @Override
    public Map<String, String> list(String prefix) {
        GetResponse resp = await(etcd.getKVClient().get(bytes(prefix), GetOption.builder().isPrefix(true).build()),
                "get prefix " + prefix);
        Map<String, String> result = new TreeMap<>();
        for (KeyValue kv : resp.getKvs()) {
            result.put(kv.getKey().toString(StandardCharsets.UTF_8), kv.getValue().toString(StandardCharsets.UTF_8));
        }
        return Collections.unmodifiableMap(result);
    }

    @Override
    public void put(String key, String value) {
        await(etcd.getKVClient().put(bytes(key), bytes(value)), "put " + key);
    }

    @Override
    public long delete(String key) {
        DeleteResponse resp = await(etcd.getKVClient().delete(bytes(key)), "delete " + key);
        return resp.getDeleted();
    }

    @Override
    public Set<String> registeredNodeUuids(String nodeType) {
        // <类型>.rpc/ 前缀下有两类键:zone/.../node_id/<n> -> NodeInfo JSON;allocated/... -> 发号哨兵(不是 JSON),跳过。
        String prefix = nodeType + ".rpc/";
        String sentinelPrefix = prefix + "allocated/";
        Set<String> uuids = new HashSet<>();
        for (Map.Entry<String, String> entry : list(prefix).entrySet()) {
            if (entry.getKey().startsWith(sentinelPrefix)) {
                continue;
            }
            try {
                JsonNode node = JSON.readTree(entry.getValue());
                JsonNode uuid = node == null ? null : node.get("nodeUuid");
                if (uuid != null && uuid.isTextual() && !uuid.asText().isEmpty()) {
                    uuids.add(uuid.asText());
                }
            } catch (IOException e) {
                // 解析不了的发现记录不算在线节点:最坏只是把一条合法的实例规则拒掉,运维仍可用 etcdctl 写。
            }
        }
        return Collections.unmodifiableSet(uuids);
    }

    private static ByteSequence bytes(String text) {
        return ByteSequence.from(text, StandardCharsets.UTF_8);
    }

    private <R> R await(CompletableFuture<R> future, String what) {
        try {
            return future.get(timeoutMs, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while waiting for etcd " + what, e);
        } catch (ExecutionException e) {
            throw new IllegalStateException("etcd " + what + " failed: " + e.getCause(), e.getCause());
        } catch (TimeoutException e) {
            future.cancel(true);
            throw new IllegalStateException("etcd " + what + " timed out after " + timeoutMs + " ms", e);
        }
    }
}
