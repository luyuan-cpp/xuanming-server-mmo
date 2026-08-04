package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KV;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import org.junit.jupiter.api.Test;

import java.util.List;
import java.util.concurrent.CompletableFuture;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

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
        Client client = mock(Client.class);
        KV kv = mock(KV.class);
        GetResponse response = mock(GetResponse.class);
        when(client.getKVClient()).thenReturn(kv);
        when(response.getKvs()).thenReturn(List.of());
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.completedFuture(response));

        GateWatcher watcher = new GateWatcher(client, properties());

        assertTrue(watcher.fetchAllGateNodes().isEmpty());
    }

    @Test
    void oneInvalidNodeInfoFailsWholeDiscoveryInsteadOfReturningPartialSnapshot() {
        Client client = mock(Client.class);
        KV kv = mock(KV.class);
        GetResponse response = mock(GetResponse.class);
        KeyValue valid = nodeRecord(
                "GateNodeService.rpc/zone/1/node_type/2/node_id/10",
                "{\"nodeId\":10,\"zoneId\":1}");
        KeyValue invalid = nodeRecord(
                "GateNodeService.rpc/zone/1/node_type/2/node_id/11",
                "not-json");
        when(client.getKVClient()).thenReturn(kv);
        when(response.getKvs()).thenReturn(List.of(valid, invalid));
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.completedFuture(response));

        GateWatcher watcher = new GateWatcher(client, properties());

        assertThrows(GateWatcher.NodeDiscoveryException.class, watcher::fetchAllGateNodes);
    }

    private static KeyValue nodeRecord(String key, String value) {
        KeyValue record = mock(KeyValue.class);
        when(record.getKey()).thenReturn(ByteSequence.from(key, java.nio.charset.StandardCharsets.UTF_8));
        when(record.getValue()).thenReturn(ByteSequence.from(value, java.nio.charset.StandardCharsets.UTF_8));
        return record;
    }

    private static GateProperties properties() {
        GateProperties properties = new GateProperties();
        properties.setDiscoveryTimeoutMs(100);
        return properties;
    }
}
