package com.game.gateway.etcd;

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
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.timeout;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link NodeInfoMirror} 的 list + watch 行为:etcd 用 Mockito 代替,watch 回调由用例手动触发,时钟手动推进。
 *
 * <p>钉住:第一次读整段读并从下一个 revision 起 watch;稳态读不碰 etcd;PUT / DELETE 事件改镜像,发号哨兵跳过;
 * watch 报错或结束后下一次读重新整段读、关掉旧 watch、丢弃旧 watch 的迟到事件;整段读失败抛异常、下一次读再试;
 * 坏记录让读失败,直到被事件改好或删掉;镜像满 {@link NodeInfoMirror#MAX_AGE_MS} 强制重新整段读;关闭后读失败。
 */
class NodeInfoMirrorTest {

    private static final String PREFIX = "GateNodeService.rpc/";
    private static final String GATE_10 = PREFIX + "zone/1/node_type/4/node_id/10";
    private static final String GATE_11 = PREFIX + "zone/2/node_type/4/node_id/11";

    private Client client;
    private KV kv;
    private Watch watch;
    private final List<Watch.Listener> listeners = new ArrayList<>();
    private final List<Watch.Watcher> watchers = new ArrayList<>();
    private final AtomicLong clock = new AtomicLong(1_000L);
    private NodeInfoMirror mirror;

    @BeforeEach
    void setUp() {
        client = mock(Client.class);
        kv = mock(KV.class);
        watch = mock(Watch.class);
        when(client.getKVClient()).thenReturn(kv);
        when(client.getWatchClient()).thenReturn(watch);
        when(watch.watch(any(ByteSequence.class), any(WatchOption.class), any(Watch.Listener.class))).thenAnswer(inv -> {
            listeners.add(inv.getArgument(2));
            Watch.Watcher w = mock(Watch.Watcher.class);
            watchers.add(w);
            return w;
        });
        mirror = new NodeInfoMirror(client, PREFIX, 100, clock::get);
    }

    /** 下一次整段读返回给定记录,header 的 revision 为 {@code revision}。 */
    private void listReturns(long revision, KeyValue... records) {
        GetResponse response = listResponse(revision, records);
        when(kv.get(any(ByteSequence.class), any(GetOption.class))).thenReturn(CompletableFuture.completedFuture(response));
    }

    /** 一份整段读的应答:给定记录,header 的 revision 为 {@code revision}。 */
    private static GetResponse listResponse(long revision, KeyValue... records) {
        GetResponse response = mock(GetResponse.class);
        Response.Header header = mock(Response.Header.class);
        when(header.getRevision()).thenReturn(revision);
        when(response.getHeader()).thenReturn(header);
        when(response.getKvs()).thenReturn(List.of(records));
        return response;
    }

    private void listFails() {
        when(kv.get(any(ByteSequence.class), any(GetOption.class)))
                .thenReturn(CompletableFuture.failedFuture(new RuntimeException("etcd unavailable")));
    }

    private static KeyValue record(String key, String json) {
        KeyValue kv = mock(KeyValue.class);
        when(kv.getKey()).thenReturn(ByteSequence.from(key, StandardCharsets.UTF_8));
        when(kv.getValue()).thenReturn(ByteSequence.from(json, StandardCharsets.UTF_8));
        return kv;
    }

    private static KeyValue gate(String key, long nodeId, long zoneId) {
        return record(key, "{\"nodeId\":" + nodeId + ",\"zoneId\":" + zoneId + "}");
    }

    private static void deliver(Watch.Listener listener, WatchEvent.EventType type, KeyValue kv) {
        WatchResponse response = mock(WatchResponse.class);
        when(response.getEvents()).thenReturn(List.of(new WatchEvent(kv, null, type)));
        listener.onNext(response);
    }

    private List<Long> nodeIds() {
        return mirror.nodes().stream().map(NodeInfoRecord::getNodeId).toList();
    }

    @Test
    void firstRead_listsThenWatchesFromNextRevision() {
        listReturns(42, gate(GATE_10, 10, 1));

        assertEquals(List.of(10L), nodeIds());

        ArgumentCaptor<WatchOption> option = ArgumentCaptor.forClass(WatchOption.class);
        verify(watch).watch(any(ByteSequence.class), option.capture(), any(Watch.Listener.class));
        assertEquals(43, option.getValue().getRevision(), "从整段读那一刻的下一个 revision 起 watch,中间的变更不会漏");
    }

    @Test
    void steadyStateReads_doNotTouchEtcd() {
        listReturns(1, gate(GATE_10, 10, 1));

        mirror.nodes();
        clock.addAndGet(NodeInfoMirror.MAX_AGE_MS - 1);
        mirror.nodes();

        verify(kv, times(1)).get(any(ByteSequence.class), any(GetOption.class));
    }

    @Test
    void watchEvents_applyPutAndDelete_andSkipAllocatorSentinel() {
        listReturns(1, gate(GATE_10, 10, 1));
        mirror.nodes();
        Watch.Listener listener = listeners.get(0);

        deliver(listener, WatchEvent.EventType.PUT, gate(GATE_11, 11, 2));
        deliver(listener, WatchEvent.EventType.PUT, record(PREFIX + "allocated/node_type/4/node_id/12", "\u0001\u0002"));
        assertEquals(List.of(10L, 11L), nodeIds(), "按键排序;发号哨兵不是 NodeInfo,跳过");

        deliver(listener, WatchEvent.EventType.DELETE, record(GATE_10, ""));
        assertEquals(List.of(11L), nodeIds());
        verify(kv, times(1)).get(any(ByteSequence.class), any(GetOption.class));
    }

    @Test
    void watchFailure_relistsOnNextRead_closesOldWatch_andDropsItsLateEvents() {
        listReturns(1, gate(GATE_10, 10, 1));
        mirror.nodes();
        Watch.Listener first = listeners.get(0);

        first.onError(new RuntimeException("stream reset"));
        listReturns(7, gate(GATE_11, 11, 2));
        assertEquals(List.of(11L), nodeIds(), "watch 出错后下一次读重新整段读");
        verify(watchers.get(0)).close();
        assertEquals(2, listeners.size(), "重建了 watch");

        deliver(first, WatchEvent.EventType.PUT, gate(GATE_10, 10, 1));
        assertEquals(List.of(11L), nodeIds(), "旧 watch 迟到的事件按代次丢弃");
    }

    @Test
    void watchCompleted_isTreatedAsStopped() {
        listReturns(1, gate(GATE_10, 10, 1));
        mirror.nodes();

        listeners.get(0).onCompleted();
        mirror.nodes();

        verify(kv, times(2)).get(any(ByteSequence.class), any(GetOption.class));
    }

    @Test
    void listFailure_throws_andNextReadRetries() {
        listFails();
        assertThrows(GateWatcher.NodeDiscoveryException.class, mirror::nodes, "读不到不能当成没有节点");

        listReturns(1, gate(GATE_10, 10, 1));
        assertEquals(List.of(10L), nodeIds());
    }

    @Test
    void invalidRecord_failsReads_untilFixedByEvent() {
        listReturns(1, gate(GATE_10, 10, 1), record(GATE_11, "not-json"));
        assertThrows(GateWatcher.NodeDiscoveryException.class, mirror::nodes, "一条坏记录让整次读失败,不返回部分列表");
        Watch.Listener listener = listeners.get(0);

        deliver(listener, WatchEvent.EventType.PUT, gate(GATE_11, 11, 2));
        assertEquals(List.of(10L, 11L), nodeIds(), "坏记录被改好之后恢复");

        deliver(listener, WatchEvent.EventType.PUT, record(GATE_10, "{broken"));
        assertThrows(GateWatcher.NodeDiscoveryException.class, mirror::nodes, "事件带来的坏记录同样让读失败");
        deliver(listener, WatchEvent.EventType.DELETE, record(GATE_10, ""));
        assertEquals(List.of(11L), nodeIds(), "坏记录被删掉之后恢复");
    }

    @Test
    void mirrorOlderThanMaxAge_isRelisted() {
        listReturns(1, gate(GATE_10, 10, 1));
        mirror.nodes();

        clock.addAndGet(NodeInfoMirror.MAX_AGE_MS);
        listReturns(9, gate(GATE_11, 11, 2));

        assertEquals(List.of(11L), nodeIds(), "watch 悄无声息停住时,镜像最多旧 MAX_AGE_MS");
        verify(watchers.get(0)).close();
    }

    @Test
    void closedMirror_failsReads_andClosesWatch() {
        listReturns(1, gate(GATE_10, 10, 1));
        mirror.nodes();

        mirror.close();

        verify(watchers.get(0)).close();
        assertThrows(GateWatcher.NodeDiscoveryException.class, mirror::nodes);
    }

    // login 的镜像由 login 发现与区服健康两个定时任务读(2026-11-12 起各有线程):第二个读者等第一个同步完直接用结果,
    // 不再各读一遍 etcd,也不会有一方以「被并发重新同步」失败。
    @Test
    void concurrentReaders_shareOneRelist() throws InterruptedException {
        NodeInfoMirror shared = new NodeInfoMirror(client, PREFIX, 10_000, clock::get);
        GetResponse response = listResponse(1, gate(GATE_10, 10, 1));
        CompletableFuture<GetResponse> pending = new CompletableFuture<>();
        when(kv.get(any(ByteSequence.class), any(GetOption.class))).thenReturn(pending);
        AtomicReference<List<NodeInfoRecord>> firstResult = new AtomicReference<>();
        AtomicReference<List<NodeInfoRecord>> secondResult = new AtomicReference<>();
        Thread first = new Thread(() -> firstResult.set(shared.nodes()), "mirror-reader-1");
        Thread second = new Thread(() -> secondResult.set(shared.nodes()), "mirror-reader-2");

        first.start();
        verify(kv, timeout(5_000)).get(any(ByteSequence.class), any(GetOption.class));
        second.start();
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
        while (second.getState() != Thread.State.BLOCKED && System.nanoTime() < deadline) {
            Thread.sleep(5);
        }
        assertEquals(Thread.State.BLOCKED, second.getState(), "第二个读者应当等在重新同步的锁上,而不是自己再整段读一遍");

        pending.complete(response);
        first.join(5_000);
        second.join(5_000);

        assertEquals(List.of(10L), nodeIdsOf(firstResult.get()));
        assertEquals(List.of(10L), nodeIdsOf(secondResult.get()));
        verify(kv, times(1)).get(any(ByteSequence.class), any(GetOption.class));
        assertEquals(1, listeners.size(), "只建了一个 watch");
    }

    private static List<Long> nodeIdsOf(List<NodeInfoRecord> nodes) {
        assertNotNull(nodes, "读者线程应当拿到结果而不是抛异常");
        return nodes.stream().map(NodeInfoRecord::getNodeId).toList();
    }
}
