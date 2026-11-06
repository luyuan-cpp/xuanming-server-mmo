package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KV;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.Response;
import io.etcd.jetcd.Watch;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import io.etcd.jetcd.options.WatchOption;
import org.junit.jupiter.api.Test;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.CompletableFuture;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

/**
 * {@link GateWatcher} 对调用方的契约:查询失败抛异常而不是空列表;真的没有登记返回空列表;一条坏记录让整次读失败。
 * 镜像的 watch 增量与重新同步见 {@link NodeInfoMirrorTest}。
 */
class GateWatcherTest {

    @Test
    void etcdFailureIsNotReportedAsEmptyNodeList() {
        Client client = mock(Client.class);
        KV kv = mock(KV.class);
        when(client.getKVClient()).thenReturn(kv);
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.failedFuture(new RuntimeException("etcd unavailable")));

        GateWatcher watcher = new GateWatcher(client, properties());

        assertThrows(GateWatcher.NodeDiscoveryException.class, watcher::fetchAllGateNodes);
    }

    @Test
    void successfulQueryWithNoRegistrationsReturnsEmptyList() {
        Client client = clientListing(List.of());

        GateWatcher watcher = new GateWatcher(client, properties());

        assertTrue(watcher.fetchAllGateNodes().isEmpty());
    }

    @Test
    void oneInvalidNodeInfoFailsWholeDiscoveryInsteadOfReturningPartialSnapshot() {
        KeyValue valid = nodeRecord(
                "GateNodeService.rpc/zone/1/node_type/2/node_id/10",
                "{\"nodeId\":10,\"zoneId\":1}");
        KeyValue invalid = nodeRecord(
                "GateNodeService.rpc/zone/1/node_type/2/node_id/11",
                "not-json");
        Client client = clientListing(List.of(valid, invalid));

        GateWatcher watcher = new GateWatcher(client, properties());

        assertThrows(GateWatcher.NodeDiscoveryException.class, watcher::fetchAllGateNodes);
    }

    /** etcd 整段读返回给定记录(revision 1),watch 返回一个不发事件的 watcher。 */
    private static Client clientListing(List<KeyValue> records) {
        Client client = mock(Client.class);
        KV kv = mock(KV.class);
        Watch watch = mock(Watch.class);
        GetResponse response = mock(GetResponse.class);
        Response.Header header = mock(Response.Header.class);
        when(header.getRevision()).thenReturn(1L);
        when(response.getHeader()).thenReturn(header);
        when(response.getKvs()).thenReturn(records);
        when(client.getKVClient()).thenReturn(kv);
        when(client.getWatchClient()).thenReturn(watch);
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.completedFuture(response));
        Watch.Watcher idle = mock(Watch.Watcher.class);
        when(watch.watch(any(ByteSequence.class), any(WatchOption.class), any(Watch.Listener.class))).thenReturn(idle);
        return client;
    }

    private static KeyValue nodeRecord(String key, String value) {
        KeyValue record = mock(KeyValue.class);
        when(record.getKey()).thenReturn(ByteSequence.from(key, StandardCharsets.UTF_8));
        when(record.getValue()).thenReturn(ByteSequence.from(value, StandardCharsets.UTF_8));
        return record;
    }

    private static GateProperties properties() {
        GateProperties properties = new GateProperties();
        properties.setDiscoveryTimeoutMs(100);
        return properties;
    }
}
