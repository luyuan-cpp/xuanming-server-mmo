package com.game.gateway.etcd;

import com.game.gateway.etcd.GateWatcher.NodeDiscoveryException;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.Watch;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.options.GetOption;
import io.etcd.jetcd.options.WatchOption;
import io.etcd.jetcd.watch.WatchEvent;
import io.etcd.jetcd.watch.WatchResponse;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.SortedSet;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.concurrent.TimeUnit;
import java.util.function.LongSupplier;

/**
 * 一个 etcd 前缀下记录的本地镜像:整段读一次建镜像,再从那一刻的 revision + 1 起 watch 增量
 * (list + watch,与 Go 的 client_rpc_router NodeWatcher 同一做法)。记录怎么解码由 {@link Decoder} 决定:
 * NodeInfo 见 {@link NodeInfoMirror},区服健康汇总见 {@link ZoneHealthWatcher}。
 *
 * <p>(2026-11-05,服务器缺口清单第 29 条)原来每次读都整段 range 一遍;改成镜像之后,稳态只收变更事件,整段读只发生在
 * 第一次读、watch 出错之后,以及每 {@link #MAX_AGE_MS} 一次。2026-11-11 从 NodeInfoMirror 抽成通用引擎,
 * 并记下每条记录的 ModRevision(读区服健康汇总时按它取最近写入的一条)。
 *
 * <p>读语义:
 * <ul>
 *   <li>镜像没同步好时,{@link #entries()} 当场整段读一次;读失败抛 {@link NodeDiscoveryException},调用方保留上一轮的结果,
 *       不把「读不到」当成「没有记录」;</li>
 *   <li>任何一条记录解码失败 → 抛同一个异常,直到那条记录被改好或删掉(整批失败,不返回部分列表);</li>
 *   <li>解码器返回 null 的键不是本镜像的记录(例如发号哨兵),跳过,不算坏记录;</li>
 *   <li>返回顺序按键排序,与 etcd 整段读的顺序相同。</li>
 * </ul>
 *
 * <p>同步规则:watch 报错或结束(含 jetcd 自己会重试的瞬时错误)一律作废这一轮镜像,下一次读重新整段读并重建 watch,
 * 旧 watch 迟到的事件按代次丢弃。另外每 {@link #MAX_AGE_MS} 强制重新整段读一次:watch 若在半开连接上悄无声息地停住,
 * 镜像最多旧这么久(Kubernetes 的 reflector 同样定期重新 list)。
 *
 * <p>线程安全:状态都在 {@link #lock} 下读写;整段读不持锁,免得最长 listTimeoutMs 的等待卡住 jetcd 的事件回调线程。
 * 同一时刻只应有一个线程读同一个镜像(各自的 {@code @Scheduled} 任务);两次重新同步撞在一起时,先发起的那次抛异常、
 * 本轮按读失败处理。
 */
final class EtcdPrefixMirror<T> implements AutoCloseable {

    /** 镜像最长用多久就强制重新整段读一次(毫秒)。 */
    static final long MAX_AGE_MS = 5 * 60_000L;

    /** 把一条记录解码成 {@code V}:返回 null = 这把键不是本镜像的记录(跳过);抛异常 = 坏记录。 */
    @FunctionalInterface
    interface Decoder<V> {
        V decode(String key, ByteSequence value) throws Exception;
    }

    /** 镜像里的一条记录:键、解码后的值、最后一次写入这把键的 etcd revision。 */
    record Entry<V>(String key, V value, long modRevision) {}

    private static final Logger log = LoggerFactory.getLogger(EtcdPrefixMirror.class);

    private final Client etcd;
    private final String prefix;
    private final ByteSequence prefixKey;
    /** 日志与异常里的记录叫法,例如 "NodeInfo"。 */
    private final String recordKind;
    private final Decoder<T> decoder;
    private final long listTimeoutMs;
    private final LongSupplier monotonicMs;

    private final Object lock = new Object();
    // 以下字段都在 lock 下读写。
    private Map<String, Entry<T>> records = new TreeMap<>();
    /** 解码失败的真实记录的键;非空时读一律失败。 */
    private SortedSet<String> badKeys = new TreeSet<>();
    private boolean synced;
    private long syncedAtMs;
    /** 每次重新整段读加一;旧 watch 的回调带着旧代次,一律丢弃。 */
    private long generation;
    private Watch.Watcher watcher;
    private boolean closed;

    /**
     * @param recordKind    日志与异常里的记录叫法
     * @param listTimeoutMs 一次整段读最长等多久
     * @param monotonicMs   单调时钟(毫秒),只用来算镜像年龄
     */
    EtcdPrefixMirror(Client etcd, String prefix, String recordKind, Decoder<T> decoder,
                     long listTimeoutMs, LongSupplier monotonicMs) {
        this.etcd = etcd;
        this.prefix = prefix;
        this.prefixKey = ByteSequence.from(prefix, StandardCharsets.UTF_8);
        this.recordKind = recordKind;
        this.decoder = decoder;
        this.listTimeoutMs = listTimeoutMs;
        this.monotonicMs = monotonicMs;
    }

    /**
     * 当前全部记录(按键排序,不可修改)。
     *
     * @throws NodeDiscoveryException 需要整段读而 etcd 查询失败,或有记录无法解码;前缀下真的没有记录返回空列表
     */
    List<Entry<T>> entries() {
        long gen;
        synchronized (lock) {
            if (closed) {
                throw new NodeDiscoveryException("etcd mirror of " + prefix + " is closed", null);
            }
            if (synced && monotonicMs.getAsLong() - syncedAtMs < MAX_AGE_MS) {
                return snapshotLocked();
            }
            gen = ++generation;
            synced = false;
            closeWatcherLocked();
        }

        GetResponse response = list();
        Map<String, Entry<T>> next = new TreeMap<>();
        SortedSet<String> bad = new TreeSet<>();
        for (KeyValue kv : response.getKvs()) {
            apply(next, bad, kv.getKey().toString(StandardCharsets.UTF_8), kv.getValue(), kv.getModRevision());
        }
        long revision = response.getHeader().getRevision();

        synchronized (lock) {
            if (closed || gen != generation) {
                throw new NodeDiscoveryException("etcd mirror of " + prefix + " was re-synchronized concurrently", null);
            }
            records = next;
            badKeys = bad;
            synced = true;
            syncedAtMs = monotonicMs.getAsLong();
            // 从整段读那一刻的下一个 revision 起 watch:两者之间的变更由 etcd 按历史补发,不会漏。
            watcher = etcd.getWatchClient().watch(prefixKey,
                    WatchOption.builder().isPrefix(true).withRevision(revision + 1).build(),
                    Watch.listener(r -> onEvents(gen, r), e -> onWatchStopped(gen, e), () -> onWatchStopped(gen, null)));
            return snapshotLocked();
        }
    }

    @Override
    public void close() {
        synchronized (lock) {
            closed = true;
            synced = false;
            closeWatcherLocked();
        }
    }

    private GetResponse list() {
        try {
            return etcd.getKVClient()
                    .get(prefixKey, GetOption.builder().isPrefix(true).build())
                    .get(listTimeoutMs, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new NodeDiscoveryException("etcd query interrupted for prefix " + prefix, e);
        } catch (Exception e) {
            log.error("Failed to fetch {} records with prefix {}: {}", recordKind, prefix, e.getMessage());
            throw new NodeDiscoveryException("etcd query failed for prefix " + prefix, e);
        }
    }

    private void onEvents(long gen, WatchResponse response) {
        synchronized (lock) {
            if (gen != generation || !synced) {
                return;
            }
            for (WatchEvent event : response.getEvents()) {
                KeyValue kv = event.getKeyValue();
                String key = kv.getKey().toString(StandardCharsets.UTF_8);
                switch (event.getEventType()) {
                    case PUT -> apply(records, badKeys, key, kv.getValue(), kv.getModRevision());
                    case DELETE -> {
                        records.remove(key);
                        badKeys.remove(key);
                    }
                    default -> {
                        // UNRECOGNIZED:新版 etcd 的未知事件类型,不改镜像。
                    }
                }
            }
        }
    }

    private void onWatchStopped(long gen, Throwable error) {
        synchronized (lock) {
            if (gen != generation || !synced) {
                return;
            }
            // 只作废,不在 jetcd 的回调里关 watcher:下一次读重新整段读时先关旧的再建新的。
            synced = false;
        }
        log.warn("etcd watch on {} stopped, re-listing on next read: {}", prefix, error == null ? "completed" : error.toString());
    }

    private void apply(Map<String, Entry<T>> into, SortedSet<String> bad, String key, ByteSequence value, long modRevision) {
        T decoded;
        try {
            decoded = decoder.decode(key, value);
        } catch (Exception e) {
            // 返回部分列表会让下游把不完整的结果发布成新的成功快照,覆盖 last-known-good:
            // 记下坏键,读一律失败,直到这条记录被改好或删掉。
            log.error("Failed to parse {} from key={}: {}", recordKind, key, e.getMessage());
            into.remove(key);
            bad.add(key);
            return;
        }
        if (decoded == null) {
            // 不是本镜像的记录(例如发号哨兵):不当坏记录、也不刷警告日志。
            return;
        }
        into.put(key, new Entry<>(key, decoded, modRevision));
        bad.remove(key);
    }

    private List<Entry<T>> snapshotLocked() {
        if (!badKeys.isEmpty()) {
            throw new NodeDiscoveryException("invalid " + recordKind + " record at key " + badKeys.first(), null);
        }
        return List.copyOf(records.values());
    }

    private void closeWatcherLocked() {
        if (watcher != null) {
            try {
                watcher.close();
            } catch (RuntimeException e) {
                log.debug("closing etcd watch on {} failed: {}", prefix, e.toString());
            }
            watcher = null;
        }
    }
}
