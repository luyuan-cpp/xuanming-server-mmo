package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import com.game.gateway.dto.AutoZoneStatus;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KV;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.Response;
import io.etcd.jetcd.Watch;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import io.etcd.jetcd.options.WatchOption;
import io.etcd.jetcd.watch.WatchEvent;
import io.etcd.jetcd.watch.WatchResponse;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * 区服健康汇总的记录格式({@link ZoneHealthRecord})与读方({@link ZoneHealthWatcher}):键形与编码往返;
 * 每个 zone 取 ModRevision 最大的那条;坏记录(键形不对、JSON 坏、键与值的 zone 不一致、UNKNOWN 状态、负数)让整次读失败;
 * 读失败抛异常而不是空结果;watch 事件增删记录。
 */
class ZoneHealthWatcherTest {

    private Client client;
    private KV kv;
    private final List<Watch.Listener> listeners = new ArrayList<>();
    private ZoneHealthWatcher watcher;

    @BeforeEach
    void setUp() {
        client = mock(Client.class);
        kv = mock(KV.class);
        Watch watch = mock(Watch.class);
        when(client.getKVClient()).thenReturn(kv);
        when(client.getWatchClient()).thenReturn(watch);
        when(watch.watch(any(ByteSequence.class), any(WatchOption.class), any(Watch.Listener.class))).thenAnswer(inv -> {
            listeners.add(inv.getArgument(2));
            return mock(Watch.Watcher.class);
        });
        GateProperties props = new GateProperties();
        props.setDiscoveryTimeoutMs(100);
        watcher = new ZoneHealthWatcher(client, props, () -> 1_000L);
    }

    private void listReturns(KeyValue... records) {
        GetResponse response = mock(GetResponse.class);
        Response.Header header = mock(Response.Header.class);
        when(header.getRevision()).thenReturn(100L);
        when(response.getHeader()).thenReturn(header);
        when(response.getKvs()).thenReturn(List.of(records));
        when(kv.get(any(ByteSequence.class), any(GetOption.class))).thenReturn(CompletableFuture.completedFuture(response));
    }

    private static KeyValue raw(String key, String value, long modRevision) {
        KeyValue kv = mock(KeyValue.class);
        when(kv.getKey()).thenReturn(ByteSequence.from(key, StandardCharsets.UTF_8));
        when(kv.getValue()).thenReturn(ByteSequence.from(value, StandardCharsets.UTF_8));
        when(kv.getModRevision()).thenReturn(modRevision);
        return kv;
    }

    private static KeyValue record(ZoneHealthRecord record, String publisher, long modRevision) {
        return raw(ZoneHealthRecord.key(record.zoneId(), publisher), record.encode(), modRevision);
    }

    private static ZoneHealthRecord health(long zoneId, AutoZoneStatus status, long online, long capacity) {
        return new ZoneHealthRecord(zoneId, status, online, capacity);
    }

    @Test
    void keyLayoutAndEncodingRoundTrip() throws Exception {
        ZoneHealthRecord original = health(101, AutoZoneStatus.DEGRADED, 1234, 36000);
        String key = ZoneHealthRecord.key(101, "0f8e2c1a-9b7d-4e2f-8a6b-3c5d7e9f1a2b");

        assertEquals("/mmorpg/zone_health/zone/101/publisher/0f8e2c1a-9b7d-4e2f-8a6b-3c5d7e9f1a2b", key);
        assertTrue(key.startsWith(ZoneHealthRecord.PREFIX), "读方 watch 的前缀要盖住记录键");
        assertEquals(original, ZoneHealthRecord.decode(key, ByteSequence.from(original.encode(), StandardCharsets.UTF_8)));
    }

    @Test
    void unknownFieldsAreIgnoredForAdditiveEvolution() throws Exception {
        String key = ZoneHealthRecord.key(7, "p1");
        String json = "{\"zoneId\":7,\"status\":\"HEALTHY\",\"onlineCount\":5,\"capacity\":10,\"addedLater\":true}";

        assertEquals(health(7, AutoZoneStatus.HEALTHY, 5, 10), ZoneHealthRecord.decode(key, ByteSequence.from(json, StandardCharsets.UTF_8)));
    }

    @Test
    void recordsRejectValuesTheyCannotDescribe() {
        assertThrows(IllegalArgumentException.class, () -> health(0, AutoZoneStatus.HEALTHY, 0, 0), "zone 号要为正");
        assertThrows(IllegalArgumentException.class, () -> health(1, AutoZoneStatus.UNKNOWN, 0, 0), "发布者从不写 UNKNOWN");
        assertThrows(IllegalArgumentException.class, () -> health(1, null, 0, 0));
        assertThrows(IllegalArgumentException.class, () -> health(1, AutoZoneStatus.HEALTHY, -1, 0));
        assertThrows(IllegalArgumentException.class, () -> health(1, AutoZoneStatus.HEALTHY, 0, -1));
        assertThrows(IllegalArgumentException.class, () -> ZoneHealthRecord.key(1, "has/slash"));
    }

    @Test
    void latestWrittenRecordWinsPerZone() {
        listReturns(
                record(health(1, AutoZoneStatus.HEALTHY, 100, 1000), "a", 50),
                record(health(1, AutoZoneStatus.DOWN, 0, 1000), "b", 60),
                record(health(2, AutoZoneStatus.DEGRADED, 7, 0), "a", 40));

        Map<Long, ZoneHealthRecord> latest = watcher.latestByZone();

        assertEquals(2, latest.size());
        assertEquals(AutoZoneStatus.DOWN, latest.get(1L).status(), "zone 1 取 ModRevision 60 的那条");
        assertEquals(AutoZoneStatus.DEGRADED, latest.get(2L).status());
    }

    @Test
    void listsTheSummaryPrefixOnly() {
        listReturns();

        assertTrue(watcher.latestByZone().isEmpty(), "真的没有记录时是空结果,不是失败");

        ArgumentCaptor<ByteSequence> key = ArgumentCaptor.forClass(ByteSequence.class);
        verify(kv).get(key.capture(), any(GetOption.class));
        assertEquals(ZoneHealthRecord.PREFIX, key.getValue().toString(StandardCharsets.UTF_8));
    }

    @Test
    void etcdFailureIsNotReportedAsNoRecords() {
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.failedFuture(new RuntimeException("etcd unavailable")));

        assertThrows(GateWatcher.NodeDiscoveryException.class, watcher::latestByZone);
    }

    @Test
    void anyBadRecordFailsTheWholeRead() {
        String good = health(1, AutoZoneStatus.HEALTHY, 1, 10).encode();
        List<KeyValue> bad = List.of(
                raw("/mmorpg/zone_health/zone/2/oops", good, 1),
                raw(ZoneHealthRecord.key(2, "p"), "{broken", 1),
                raw(ZoneHealthRecord.key(2, "p"), good, 1),
                raw(ZoneHealthRecord.key(2, "p"), "{\"zoneId\":2,\"status\":\"UNKNOWN\",\"onlineCount\":0,\"capacity\":0}", 1),
                raw(ZoneHealthRecord.key(2, "p"), "{\"zoneId\":2,\"status\":\"MAYBE\",\"onlineCount\":0,\"capacity\":0}", 1),
                raw(ZoneHealthRecord.key(2, "p"), "{\"zoneId\":2,\"status\":\"DOWN\",\"onlineCount\":-5,\"capacity\":0}", 1));
        for (KeyValue badRecord : bad) {
            setUp();
            listReturns(record(health(1, AutoZoneStatus.HEALTHY, 1, 10), "p", 1), badRecord);
            assertThrows(GateWatcher.NodeDiscoveryException.class, watcher::latestByZone,
                    "坏记录 " + badRecord.getKey().toString(StandardCharsets.UTF_8) + " 必须让整次读失败,不返回部分结果");
        }
    }

    @Test
    void watchEventsAddAndRemoveRecords() {
        listReturns(record(health(1, AutoZoneStatus.HEALTHY, 1, 10), "a", 10));
        watcher.latestByZone();
        Watch.Listener listener = listeners.get(0);

        deliver(listener, WatchEvent.EventType.PUT, record(health(1, AutoZoneStatus.DOWN, 0, 10), "b", 20));
        assertEquals(AutoZoneStatus.DOWN, watcher.latestByZone().get(1L).status(), "另一个发布者后写的那条胜出");

        deliver(listener, WatchEvent.EventType.DELETE, raw(ZoneHealthRecord.key(1, "b"), "", 30));
        assertEquals(AutoZoneStatus.HEALTHY, watcher.latestByZone().get(1L).status(), "它的记录随租约消失后,回到剩下的那条");

        deliver(listener, WatchEvent.EventType.DELETE, raw(ZoneHealthRecord.key(1, "a"), "", 31));
        assertTrue(watcher.latestByZone().isEmpty(), "两个发布者都没了,这个 zone 没有记录");
    }

    private static void deliver(Watch.Listener listener, WatchEvent.EventType type, KeyValue kv) {
        WatchResponse response = mock(WatchResponse.class);
        when(response.getEvents()).thenReturn(List.of(new WatchEvent(kv, null, type)));
        listener.onNext(response);
    }
}
